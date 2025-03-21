package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	endpoint "net/http/pprof"
	"os"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	health "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"

	"github.com/operator-framework/operator-registry/pkg/api"
	"github.com/operator-framework/operator-registry/pkg/cache"
	"github.com/operator-framework/operator-registry/pkg/lib/dns"
	"github.com/operator-framework/operator-registry/pkg/lib/log"
	"github.com/operator-framework/operator-registry/pkg/server"
)

type serve struct {
	configDir             string
	cacheDir              string
	cacheOnly             bool
	cacheEnforceIntegrity bool

	port           string
	terminationLog string

	debug           bool
	pprofAddr       string
	captureProfiles bool

	logger *logrus.Entry
}

const (
	defaultCPUStartupPath string = "/debug/pprof/startup/cpu"
)

func NewCmd() *cobra.Command {
	logger := logrus.New()
	s := serve{
		logger: logrus.NewEntry(logger),
	}
	cmd := &cobra.Command{
		Use:   "serve <source_path>",
		Short: "serve declarative configs",
		Long: `This command serves declarative configs via a GRPC server.

NOTE: The declarative config directory is loaded by the serve command at
startup. Changes made to the declarative config after the this command starts
will not be reflected in the served content.
`,
		Args: cobra.ExactArgs(1),
		PreRun: func(_ *cobra.Command, args []string) {
			s.configDir = args[0]
			if s.debug {
				logger.SetLevel(logrus.DebugLevel)
			}
		},
		Run: func(cmd *cobra.Command, _ []string) {
			if !cmd.Flags().Changed("cache-enforce-integrity") {
				s.cacheEnforceIntegrity = s.cacheDir != "" && !s.cacheOnly
			}
			if err := s.run(cmd.Context()); err != nil {
				logger.Fatal(err)
			}
		},
	}

	cmd.Flags().BoolVar(&s.debug, "debug", false, "enable debug logging")
	cmd.Flags().StringVarP(&s.terminationLog, "termination-log", "t", "/dev/termination-log", "path to a container termination log file")
	cmd.Flags().StringVarP(&s.port, "port", "p", "50051", "port number to serve on")
	cmd.Flags().StringVar(&s.pprofAddr, "pprof-addr", "localhost:6060", "address of startup profiling endpoint (addr:port format)")
	cmd.Flags().BoolVar(&s.captureProfiles, "pprof-capture-profiles", false, "capture pprof CPU profiles")
	cmd.Flags().StringVar(&s.cacheDir, "cache-dir", "", "if set, sync and persist server cache directory")
	cmd.Flags().BoolVar(&s.cacheOnly, "cache-only", false, "sync the serve cache and exit without serving")
	cmd.Flags().BoolVar(&s.cacheEnforceIntegrity, "cache-enforce-integrity", false, "exit with error if cache is not present or has been invalidated. (default: true when --cache-dir is set and --cache-only is false, false otherwise), ")
	return cmd
}

func (s *serve) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mainLogger := s.logger.Dup()
	p := newProfilerInterface(s.pprofAddr, mainLogger)
	if err := p.startEndpoint(); err != nil {
		return fmt.Errorf("could not start pprof endpoint: %v", err)
	}
	if s.captureProfiles {
		if err := p.startCPUProfileCache(); err != nil {
			return fmt.Errorf("could not start CPU profile: %v", err)
		}
	}

	// Immediately set up termination log
	err := log.AddDefaultWriterHooks(s.terminationLog)
	if err != nil {
		mainLogger.WithError(err).Warn("unable to set termination log path")
	}

	// Ensure there is a default nsswitch config
	if err := dns.EnsureNsswitch(); err != nil {
		mainLogger.WithError(err).Warn("unable to write default nsswitch config")
	}

	if s.cacheDir == "" && s.cacheEnforceIntegrity {
		return fmt.Errorf("--cache-dir must be specified with --cache-enforce-integrity")
	}

	if s.cacheDir == "" {
		s.cacheDir, err = os.MkdirTemp("", "opm-serve-cache-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(s.cacheDir)
	}
	mainLogger = mainLogger.WithFields(logrus.Fields{
		"configs": s.configDir,
		"cache":   s.cacheDir,
	})

	store, err := cache.New(s.cacheDir, cache.WithLog(mainLogger))
	if err != nil {
		return err
	}
	defer store.Close()
	if s.cacheEnforceIntegrity {
		if err := store.CheckIntegrity(ctx, os.DirFS(s.configDir)); err != nil {
			return fmt.Errorf("integrity check failed: %v", err)
		}
		if err := store.Load(ctx); err != nil {
			return fmt.Errorf("failed to load cache: %v", err)
		}
	} else {
		if err := cache.LoadOrRebuild(ctx, store, os.DirFS(s.configDir)); err != nil {
			return fmt.Errorf("failed to load or rebuild cache: %v", err)
		}
	}

	if s.cacheOnly {
		return nil
	}

	mainLogger = mainLogger.WithFields(logrus.Fields{"port": s.port})

	lis, err := net.Listen("tcp", ":"+s.port)
	if err != nil {
		return fmt.Errorf("failed to listen: %s", err)
	}

	streamLogger, unaryLogger := loggingInterceptors(s.logger.Dup())
	grpcServer := grpc.NewServer(
		grpc.ChainStreamInterceptor(streamLogger),
		grpc.ChainUnaryInterceptor(unaryLogger),
	)
	api.RegisterRegistryServer(grpcServer, server.NewRegistryServer(store))
	health.RegisterHealthServer(grpcServer, server.NewHealthServer())
	reflection.Register(grpcServer)
	mainLogger.Info("serving registry")
	p.stopCPUProfileCache()

	go func() {
		<-ctx.Done()
		mainLogger.Info("shutting down server")
		grpcServer.GracefulStop()
		if err := p.stopEndpoint(ctx); err != nil {
			mainLogger.Warnf("error shutting down pprof server: %v", err)
		}
	}()

	return grpcServer.Serve(lis)
}

// manages an HTTP pprof endpoint served by `server`,
// including default pprof handlers and custom cpu pprof cache stored in `cache`.
// the cache is intended to sample CPU activity for a period and serve the data
// via a custom pprof path once collection is complete (e.g. over process initialization)
type profilerInterface struct {
	addr  string
	cache bytes.Buffer

	server http.Server

	cacheReady bool
	cacheLock  sync.RWMutex

	logger   *logrus.Entry
	closeErr chan error
}

func newProfilerInterface(a string, log *logrus.Entry) *profilerInterface {
	return &profilerInterface{
		addr:   a,
		logger: log.WithFields(logrus.Fields{"address": a}),
		cache:  bytes.Buffer{},
	}
}

func (p *profilerInterface) isEnabled() bool {
	return p.addr != ""
}

func (p *profilerInterface) startEndpoint() error {
	// short-circuit if not enabled
	if !p.isEnabled() {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", endpoint.Index)
	mux.HandleFunc("/debug/pprof/cmdline", endpoint.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", endpoint.Profile)
	mux.HandleFunc("/debug/pprof/symbol", endpoint.Symbol)
	mux.HandleFunc("/debug/pprof/trace", endpoint.Trace)
	mux.HandleFunc(defaultCPUStartupPath, p.httpHandler)

	p.server = http.Server{
		Addr:         p.addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	lis, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}

	p.closeErr = make(chan error)
	go func() {
		p.closeErr <- func() error {
			p.logger.Info("starting pprof endpoint")
			if err := p.server.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}()
	}()
	return nil
}

func (p *profilerInterface) startCPUProfileCache() error {
	// short-circuit if not enabled
	if !p.isEnabled() {
		return nil
	}

	p.logger.Infof("start caching cpu profile data at %q", defaultCPUStartupPath)
	if err := pprof.StartCPUProfile(&p.cache); err != nil {
		return err
	}

	return nil
}

func (p *profilerInterface) stopCPUProfileCache() {
	// short-circuit if not enabled
	if !p.isEnabled() {
		return
	}
	pprof.StopCPUProfile()
	p.setCacheReady()
	p.logger.Info("stopped caching cpu profile data")
}

func (p *profilerInterface) httpHandler(w http.ResponseWriter, r *http.Request) {
	if !p.isCacheReady() {
		http.Error(w, "cpu profile cache is not yet ready", http.StatusServiceUnavailable)
	}
	_, _ = w.Write(p.cache.Bytes())
}

func (p *profilerInterface) stopEndpoint(ctx context.Context) error {
	if !p.isEnabled() {
		return nil
	}
	if err := p.server.Shutdown(ctx); err != nil {
		return err
	}
	return <-p.closeErr
}

func (p *profilerInterface) isCacheReady() bool {
	p.cacheLock.RLock()
	isReady := p.cacheReady
	p.cacheLock.RUnlock()

	return isReady
}

func (p *profilerInterface) setCacheReady() {
	p.cacheLock.Lock()
	p.cacheReady = true
	p.cacheLock.Unlock()
}

func loggingInterceptors(logger *logrus.Entry) (grpc.StreamServerInterceptor, grpc.UnaryServerInterceptor) {
	requestLogger := logger.Dup()
	requestLoggerOpts := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
		logging.WithFieldsFromContext(func(ctx context.Context) logging.Fields {
			fields := logging.ExtractFields(ctx)
			metadataFields := logging.Fields{}
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				for k, v := range md {
					metadataFields = append(metadataFields, k, v)
				}
				fields = fields.AppendUnique(metadataFields)
			}
			return fields
		}),
	}
	return logging.StreamServerInterceptor(interceptorLogger(requestLogger), requestLoggerOpts...),
		logging.UnaryServerInterceptor(interceptorLogger(requestLogger), requestLoggerOpts...)
}

func interceptorLogger(l *logrus.Entry) logging.Logger {
	return logging.LoggerFunc(func(_ context.Context, lvl logging.Level, msg string, fields ...any) {
		f := make(map[string]any, len(fields)/2)
		i := logging.Fields(fields).Iterator()
		for i.Next() {
			k, v := i.At()
			f[k] = v
		}
		l := l.WithFields(f)

		switch lvl {
		case logging.LevelDebug:
			l.Debug(msg)
		case logging.LevelInfo:
			l.Info(msg)
		case logging.LevelWarn:
			l.Warn(msg)
		case logging.LevelError:
			l.Error(msg)
		default:
			panic(fmt.Sprintf("unknown level %v", lvl))
		}
	})
}
