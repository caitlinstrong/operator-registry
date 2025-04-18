package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	"github.com/operator-framework/operator-registry/alpha/action"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/pkg/api"
	fbccache "github.com/operator-framework/operator-registry/pkg/cache"
	"github.com/operator-framework/operator-registry/pkg/registry"
	"github.com/operator-framework/operator-registry/pkg/sqlite"
)

const (
	dbPort    = ":50052"
	dbAddress = "localhost" + dbPort
	dbName    = "test.db"

	cachePort    = ":50053"
	cacheAddress = "localhost" + cachePort

	deprecationCachePort    = ":50054"
	deprecationCacheAddress = "localhost" + deprecationCachePort
)

func createDBStore(dbPath string) *sqlite.SQLQuerier {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		logrus.Fatal(err)
	}
	load, err := sqlite.NewSQLLiteLoader(db)
	if err != nil {
		logrus.Fatal(err)
	}
	if err := load.Migrate(context.TODO()); err != nil {
		logrus.Fatal(err)
	}

	loader := sqlite.NewSQLLoaderForDirectory(load, "../../manifests")
	if err := loader.Populate(); err != nil {
		logrus.Fatal(err)
	}
	if _, err := db.Exec("UPDATE operatorbundle SET bundlepath = 'fake/etcd-operator:v0.9.2' WHERE name = 'etcdoperator.v0.9.2'"); err != nil {
		logrus.Fatal(err)
	}
	if err := db.Close(); err != nil {
		logrus.Fatal(err)
	}
	store, err := sqlite.NewSQLLiteQuerier(dbPath, sqlite.OmitManifests(true))
	if err != nil {
		logrus.Fatal(err)
	}
	return store
}

func fbcCache(catalogDir, cacheDir string) (fbccache.Cache, error) {
	store, err := fbccache.New(cacheDir)
	if err != nil {
		return nil, err
	}
	if err := store.Build(context.Background(), os.DirFS(catalogDir)); err != nil {
		return nil, err
	}
	if err := store.Load(context.Background()); err != nil {
		return nil, err
	}
	return store, nil
}

func fbcCacheFromFs(catalogFS fs.FS, cacheDir string) (fbccache.Cache, error) {
	store, err := fbccache.New(cacheDir)
	if err != nil {
		return nil, err
	}
	if err := store.Build(context.Background(), catalogFS); err != nil {
		return nil, err
	}
	if err := store.Load(context.Background()); err != nil {
		return nil, err
	}
	return store, nil
}

func server(store registry.GRPCQuery) *grpc.Server {
	s := grpc.NewServer()
	api.RegisterRegistryServer(s, NewRegistryServer(store))
	return s
}

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "operator-registry-server-test-")
	if err != nil {
		logrus.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			logrus.Fatalf("couldn't remove test directory: %v", err)
		}
	}()

	dbFile := filepath.Join(tmpDir, "test.db")
	dbStore := createDBStore(dbFile)

	fbcDir := filepath.Join(tmpDir, "fbc")
	fbcMigrate := action.Migrate{
		CatalogRef: dbFile,
		OutputDir:  fbcDir,
		WriteFunc:  declcfg.WriteJSON,
		FileExt:    ".json",
	}
	if err := fbcMigrate.Run(context.TODO()); err != nil {
		logrus.Fatal(err)
	}

	grpcServer := server(dbStore)

	fbcStore, err := fbcCache(fbcDir, filepath.Join(tmpDir, "cache"))
	if err != nil {
		logrus.Fatalf("failed to create cache: %v", err)
	}
	fbcServerSimple := server(fbcStore)

	fbcDeprecationStore, err := fbcCacheFromFs(validFS, filepath.Join(tmpDir, "deprecation-cache"))
	if err != nil {
		logrus.Fatalf("failed to create deprecation cache: %v", err)
	}
	fbcServerDeprecations := server(fbcDeprecationStore)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		lis, err := net.Listen("tcp", fmt.Sprintf("localhost%s", dbPort))
		if err != nil {
			logrus.Fatalf("failed to listen: %v", err)
		}
		wg.Done()
		if err := grpcServer.Serve(lis); err != nil {
			logrus.Fatalf("failed to serve db: %v", err)
		}
	}()
	go func() {
		lis, err := net.Listen("tcp", fmt.Sprintf("localhost%s", cachePort))
		if err != nil {
			logrus.Fatalf("failed to listen: %v", err)
		}
		wg.Done()
		if err := fbcServerSimple.Serve(lis); err != nil {
			logrus.Fatalf("failed to serve fbc cache: %v", err)
		}
	}()
	go func() {
		lis, err := net.Listen("tcp", deprecationCacheAddress)
		if err != nil {
			logrus.Fatalf("failed to listen: %v", err)
		}
		wg.Done()
		if err := fbcServerDeprecations.Serve(lis); err != nil {
			logrus.Fatalf("failed to serve fbc cache: %v", err)
		}
	}()
	wg.Wait()
	exit := m.Run()
	os.Exit(exit)
}

func client(t *testing.T, address string) (api.RegistryClient, *grpc.ClientConn) {
	// nolint:staticcheck
	conn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		t.Fatalf("did not connect: %v", err)
	}

	ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
	conn.WaitForStateChange(ctx, connectivity.TransientFailure)

	return api.NewRegistryClient(conn), conn
}

func TestListPackages(t *testing.T) {
	var (
		listPackagesExpected    = []string{"etcd", "prometheus", "strimzi-kafka-operator"}
		listPackagesExpectedDep = []string{"cockroachdb"}
	)

	t.Run("Sqlite", testListPackages(dbAddress, listPackagesExpected))
	t.Run("FBCCache", testListPackages(cacheAddress, listPackagesExpected))
	t.Run("FBCCacheWithDeprecations", testListPackages(deprecationCacheAddress, listPackagesExpectedDep))
}

func testListPackages(addr string, expected []string) func(*testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		stream, err := c.ListPackages(context.TODO(), &api.ListPackageRequest{})
		require.NoError(t, err)

		packages := []string{}
		waitc := make(chan struct{})
		go func(t *testing.T) {
			for {
				in, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					// read done.
					close(waitc)
					return
				}
				require.NoError(t, err)
				packages = append(packages, in.Name)
			}
		}(t)
		<-waitc
		require.ElementsMatch(t, expected, packages)
	}
}

func TestGetPackage(t *testing.T) {
	var (
		getPackageExpected = &api.Package{
			Name: "etcd",
			Channels: []*api.Channel{
				{
					Name:    "alpha",
					CsvName: "etcdoperator.v0.9.2",
				},
				{
					Name:    "beta",
					CsvName: "etcdoperator.v0.9.0",
				},
				{
					Name:    "stable",
					CsvName: "etcdoperator.v0.9.2",
				},
			},
			DefaultChannelName: "alpha",
		}

		getPackageExpectedDep = &api.Package{
			Name: "cockroachdb",
			Channels: []*api.Channel{
				{
					Name:    "stable-5.x",
					CsvName: "cockroachdb.v5.0.4",
					Deprecation: &api.Deprecation{
						Message: `channel stable-5.x is no longer supported.  Please switch to channel 'stable-6.x'.
`,
					},
				},
				{
					Name:    "stable-v6.x",
					CsvName: "cockroachdb.v6.0.0",
				},
			},
			DefaultChannelName: "stable-v6.x",
			Deprecation: &api.Deprecation{
				Message: `package cockroachdb is end of life.  Please use 'nouveau-cockroachdb' package for support.
`,
			},
		}
	)
	t.Run("Sqlite", testGetPackage(dbAddress, getPackageExpected))
	t.Run("FBCCache", testGetPackage(cacheAddress, getPackageExpected))
	t.Run("FBCCacheWithDeprecations", testGetPackage(deprecationCacheAddress, getPackageExpectedDep))
}

func testGetPackage(addr string, expected *api.Package) func(*testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		pkg, err := c.GetPackage(context.TODO(), &api.GetPackageRequest{Name: expected.Name})
		require.NoError(t, err)

		opts := []cmp.Option{
			cmpopts.IgnoreUnexported(api.Package{}),
			cmpopts.IgnoreUnexported(api.Channel{}),
			cmpopts.IgnoreUnexported(api.Deprecation{}),
			cmpopts.SortSlices(func(x, y *api.Channel) bool {
				return x.Name < y.Name
			}),
		}
		require.True(t, cmp.Equal(expected, pkg, opts...), cmp.Diff(expected, pkg, opts...))
	}
}

func TestGetBundle(t *testing.T) {
	var (
		cockroachBundle = &api.Bundle{
			CsvName:      "cockroachdb.v5.0.4",
			PackageName:  "cockroachdb",
			ChannelName:  "stable-5.x",
			CsvJson:      "",
			BundlePath:   "quay.io/openshift-community-operators/cockroachdb@sha256:f42337e7b85a46d83c94694638e2312e10ca16a03542399a65ba783c94a32b63",
			RequiredApis: nil,
			Version:      "5.0.4",
			SkipRange:    "",
			Dependencies: []*api.Dependency(nil),
			ProvidedApis: []*api.GroupVersionKind{
				{Group: "charts.operatorhub.io", Version: "v1alpha1", Kind: "Cockroachdb"},
			},
			Properties: []*api.Property{
				{
					Type:  "olm.gvk",
					Value: "{\"group\":\"charts.operatorhub.io\",\"kind\":\"Cockroachdb\",\"version\":\"v1alpha1\"}",
				},
				{
					Type:  "olm.package",
					Value: "{\"packageName\":\"cockroachdb\",\"version\":\"5.0.4\"}",
				},
			},
		}
	)
	t.Run("Sqlite", testGetBundle(dbAddress, etcdoperatorV0_9_2("alpha", false, false, includeManifestsAll)))
	t.Run("FBCCache", testGetBundle(cacheAddress, etcdoperatorV0_9_2("alpha", false, true, includeManifestsAll)))
	t.Run("FBCCacheWithDeprecations", testGetBundle(deprecationCacheAddress, cockroachBundle))
}

func testGetBundle(addr string, expected *api.Bundle) func(*testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		bundle, err := c.GetBundle(context.TODO(), &api.GetBundleRequest{PkgName: expected.PackageName, ChannelName: expected.ChannelName, CsvName: expected.CsvName})
		require.NoError(t, err)

		EqualBundles(t, *expected, *bundle)
	}
}

func TestGetBundleForChannel(t *testing.T) {
	{
		b := etcdoperatorV0_9_2("alpha", false, false, includeManifestsAll)
		t.Run("Sqlite", testGetBundleForChannel(dbAddress, &api.Bundle{
			CsvName: b.CsvName,
			CsvJson: b.CsvJson + "\n",
		}))
	}
	t.Run("FBCCache", testGetBundleForChannel(cacheAddress, etcdoperatorV0_9_2("alpha", false, true, includeManifestsAll)))
}

func testGetBundleForChannel(addr string, expected *api.Bundle) func(*testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		// nolint:staticcheck // ignore this, since we still want to test it even if marked deprecated
		bundle, err := c.GetBundleForChannel(context.TODO(), &api.GetBundleInChannelRequest{PkgName: "etcd", ChannelName: "alpha"})
		require.NoError(t, err)
		EqualBundles(t, *expected, *bundle)
	}
}

func TestGetChannelEntriesThatReplace(t *testing.T) {
	var (
		getChannelEntriesThatReplaceExpected = []*api.ChannelEntry{
			{
				PackageName: "etcd",
				ChannelName: "alpha",
				BundleName:  "etcdoperator.v0.9.0",
				Replaces:    "etcdoperator.v0.6.1",
			},
			{
				PackageName: "etcd",
				ChannelName: "beta",
				BundleName:  "etcdoperator.v0.9.0",
				Replaces:    "etcdoperator.v0.6.1",
			},
			{
				PackageName: "etcd",
				ChannelName: "stable",
				BundleName:  "etcdoperator.v0.9.0",
				Replaces:    "etcdoperator.v0.6.1",
			},
		}

		getChannelEntriesThatReplaceExpectedDep = []*api.ChannelEntry{
			{
				PackageName: "cockroachdb",
				ChannelName: "stable-5.x",
				BundleName:  "cockroachdb.v5.0.4",
				Replaces:    "cockroachdb.v5.0.3",
			},
		}
	)

	t.Run("Sqlite", testGetChannelEntriesThatReplace(dbAddress, getChannelEntriesThatReplaceExpected))
	t.Run("FBCCache", testGetChannelEntriesThatReplace(cacheAddress, getChannelEntriesThatReplaceExpected))
	t.Run("FBCCacheWithDeprecations", testGetChannelEntriesThatReplace(deprecationCacheAddress, getChannelEntriesThatReplaceExpectedDep))
}

func testGetChannelEntriesThatReplace(addr string, expected []*api.ChannelEntry) func(*testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		stream, err := c.GetChannelEntriesThatReplace(context.TODO(), &api.GetAllReplacementsRequest{CsvName: expected[0].Replaces})
		require.NoError(t, err)

		channelEntries := []*api.ChannelEntry{}
		waitc := make(chan struct{})
		go func(t *testing.T) {
			for {
				in, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					// read done.
					close(waitc)
					return
				}
				if err != nil {
					t.Error(err)
					close(waitc)
					return
				}
				channelEntries = append(channelEntries, in)
			}
		}(t)
		<-waitc

		opts := []cmp.Option{
			cmpopts.IgnoreUnexported(api.ChannelEntry{}),
			cmpopts.SortSlices(func(x, y *api.ChannelEntry) bool {
				if x.PackageName != y.PackageName {
					return x.PackageName < y.PackageName
				}
				if x.ChannelName != y.ChannelName {
					return x.ChannelName < y.ChannelName
				}
				if x.BundleName != y.BundleName {
					return x.BundleName < y.BundleName
				}
				if x.Replaces != y.Replaces {
					return x.Replaces < y.Replaces
				}
				return false
			}),
		}

		require.Truef(t, cmp.Equal(expected, channelEntries, opts...), cmp.Diff(expected, channelEntries, opts...))
	}
}

func TestGetBundleThatReplaces(t *testing.T) {
	t.Run("Sqlite", testGetBundleThatReplaces(dbAddress, etcdoperatorV0_9_2("alpha", false, false, includeManifestsAll)))
	t.Run("FBCCache", testGetBundleThatReplaces(cacheAddress, etcdoperatorV0_9_2("alpha", false, true, includeManifestsAll)))
}

func testGetBundleThatReplaces(addr string, expected *api.Bundle) func(*testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		bundle, err := c.GetBundleThatReplaces(context.TODO(), &api.GetReplacementRequest{CsvName: "etcdoperator.v0.9.0", PkgName: "etcd", ChannelName: "alpha"})
		require.NoError(t, err)
		EqualBundles(t, *expected, *bundle)
	}
}

func TestGetBundleThatReplacesSynthetic(t *testing.T) {
	t.Run("Sqlite", testGetBundleThatReplacesSynthetic(dbAddress, etcdoperatorV0_9_2("alpha", false, false, includeManifestsAll)))
	t.Run("FBCCache", testGetBundleThatReplacesSynthetic(cacheAddress, etcdoperatorV0_9_2("alpha", false, true, includeManifestsAll)))
}

func testGetBundleThatReplacesSynthetic(addr string, expected *api.Bundle) func(*testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		// 0.9.1 is not actually a bundle in the registry
		bundle, err := c.GetBundleThatReplaces(context.TODO(), &api.GetReplacementRequest{CsvName: "etcdoperator.v0.9.1", PkgName: "etcd", ChannelName: "alpha"})
		require.NoError(t, err)
		EqualBundles(t, *expected, *bundle)
	}
}

func TestGetChannelEntriesThatProvide(t *testing.T) {
	t.Run("Sqlite", testGetChannelEntriesThatProvide(dbAddress))
	t.Run("FBCCache", testGetChannelEntriesThatProvide(cacheAddress))
}

func testGetChannelEntriesThatProvide(addr string) func(t *testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		stream, err := c.GetChannelEntriesThatProvide(context.TODO(), &api.GetAllProvidersRequest{Group: "etcd.database.coreos.com", Version: "v1beta2", Kind: "EtcdCluster"})
		require.NoError(t, err)

		channelEntries := []api.ChannelEntry{}
		waitc := make(chan struct{})
		go func(t *testing.T) {
			for {
				in, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					// read done.
					close(waitc)
					return
				}
				if err != nil {
					t.Error(err)
					close(waitc)
					return
				}
				channelEntries = append(channelEntries, *in)
			}
		}(t)
		<-waitc

		expected := []api.ChannelEntry{
			{
				PackageName: "etcd",
				ChannelName: "alpha",
				BundleName:  "etcdoperator.v0.6.1",
				Replaces:    "",
			},
			{
				PackageName: "etcd",
				ChannelName: "alpha",
				BundleName:  "etcdoperator.v0.9.0",
				Replaces:    "etcdoperator.v0.6.1",
			},
			{
				PackageName: "etcd",
				ChannelName: "alpha",
				BundleName:  "etcdoperator.v0.9.2",
				Replaces:    "etcdoperator.v0.9.1",
			},
			{
				PackageName: "etcd",
				ChannelName: "alpha",
				BundleName:  "etcdoperator.v0.9.2",
				Replaces:    "etcdoperator.v0.9.0",
			},
			{
				PackageName: "etcd",
				ChannelName: "beta",
				BundleName:  "etcdoperator.v0.6.1",
				Replaces:    "",
			},
			{
				PackageName: "etcd",
				ChannelName: "beta",
				BundleName:  "etcdoperator.v0.9.0",
				Replaces:    "etcdoperator.v0.6.1",
			},
			{
				PackageName: "etcd",
				ChannelName: "stable",
				BundleName:  "etcdoperator.v0.6.1",
				Replaces:    "",
			},
			{
				PackageName: "etcd",
				ChannelName: "stable",
				BundleName:  "etcdoperator.v0.9.0",
				Replaces:    "etcdoperator.v0.6.1",
			},
			{
				PackageName: "etcd",
				ChannelName: "stable",
				BundleName:  "etcdoperator.v0.9.2",
				Replaces:    "etcdoperator.v0.9.1",
			},
			{
				PackageName: "etcd",
				ChannelName: "stable",
				BundleName:  "etcdoperator.v0.9.2",
				Replaces:    "etcdoperator.v0.9.0",
			},
		}
		opts := []cmp.Option{
			cmpopts.IgnoreUnexported(api.ChannelEntry{}),
			cmpopts.SortSlices(func(x, y api.ChannelEntry) bool {
				if x.PackageName != y.PackageName {
					return x.PackageName < y.PackageName
				}
				if x.ChannelName != y.ChannelName {
					return x.ChannelName < y.ChannelName
				}
				if x.BundleName != y.BundleName {
					return x.BundleName < y.BundleName
				}
				if x.Replaces != y.Replaces {
					return x.Replaces < y.Replaces
				}
				return false
			}),
		}
		require.Truef(t, cmp.Equal(expected, channelEntries, opts...), cmp.Diff(expected, channelEntries, opts...))
	}
}

func TestGetLatestChannelEntriesThatProvide(t *testing.T) {
	t.Run("Sqlite", testGetLatestChannelEntriesThatProvide(dbAddress))
	t.Run("FBCCache", testGetLatestChannelEntriesThatProvide(cacheAddress))
}

func testGetLatestChannelEntriesThatProvide(addr string) func(t *testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		stream, err := c.GetLatestChannelEntriesThatProvide(context.TODO(), &api.GetLatestProvidersRequest{Group: "etcd.database.coreos.com", Version: "v1beta2", Kind: "EtcdCluster"})
		require.NoError(t, err)

		channelEntries := []*api.ChannelEntry{}
		waitc := make(chan struct{})
		go func(t *testing.T) {
			for {
				in, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					// read done.
					close(waitc)
					return
				}
				if err != nil {
					t.Error(err)
					close(waitc)
					return
				}
				channelEntries = append(channelEntries, in)
			}
		}(t)
		<-waitc

		expected := []*api.ChannelEntry{
			{
				PackageName: "etcd",
				ChannelName: "alpha",
				BundleName:  "etcdoperator.v0.9.2",
				Replaces:    "etcdoperator.v0.9.0",
			},
			{
				PackageName: "etcd",
				ChannelName: "beta",
				BundleName:  "etcdoperator.v0.9.0",
				Replaces:    "etcdoperator.v0.6.1",
			},
			{
				PackageName: "etcd",
				ChannelName: "stable",
				BundleName:  "etcdoperator.v0.9.2",
				Replaces:    "etcdoperator.v0.9.0",
			},
		}

		opts := []cmp.Option{
			cmpopts.IgnoreUnexported(api.ChannelEntry{}),
			cmpopts.SortSlices(func(x, y *api.ChannelEntry) bool {
				if x.PackageName != y.PackageName {
					return x.PackageName < y.PackageName
				}
				if x.ChannelName != y.ChannelName {
					return x.ChannelName < y.ChannelName
				}
				if x.BundleName != y.BundleName {
					return x.BundleName < y.BundleName
				}
				if x.Replaces != y.Replaces {
					return x.Replaces < y.Replaces
				}
				return false
			}),
		}
		require.Truef(t, cmp.Equal(expected, channelEntries, opts...), cmp.Diff(expected, channelEntries, opts...))
	}
}

func TestGetDefaultBundleThatProvides(t *testing.T) {
	t.Run("Sqlite", testGetDefaultBundleThatProvides(dbAddress, etcdoperatorV0_9_2("alpha", false, false, includeManifestsAll)))
	t.Run("FBCCache", testGetDefaultBundleThatProvides(cacheAddress, etcdoperatorV0_9_2("alpha", false, true, includeManifestsAll)))
}

func testGetDefaultBundleThatProvides(addr string, expected *api.Bundle) func(*testing.T) {
	return func(t *testing.T) {
		c, conn := client(t, addr)
		defer conn.Close()

		bundle, err := c.GetDefaultBundleThatProvides(context.TODO(), &api.GetDefaultProviderRequest{Group: "etcd.database.coreos.com", Version: "v1beta2", Kind: "EtcdCluster"})
		require.NoError(t, err)
		EqualBundles(t, *expected, *bundle)
	}
}

func TestListBundles(t *testing.T) {
	t.Run("Sqlite", testListBundles(dbAddress,
		etcdoperatorV0_9_2("alpha", true, false, includeManifestsNone),
		etcdoperatorV0_9_2("stable", true, false, includeManifestsNone)))
	t.Run("FBCCache", testListBundles(cacheAddress,
		etcdoperatorV0_9_2("alpha", true, true, includeManifestsNone),
		etcdoperatorV0_9_2("stable", true, true, includeManifestsNone)))
}

func testListBundles(addr string, etcdAlpha *api.Bundle, etcdStable *api.Bundle) func(*testing.T) {
	return func(t *testing.T) {
		require := require.New(t)

		c, conn := client(t, addr)
		defer conn.Close()

		stream, err := c.ListBundles(context.TODO(), &api.ListBundlesRequest{})
		require.NoError(err)

		expected := []string{
			"etcdoperator.v0.6.1",
			"prometheusoperator.0.22.2",
			"strimzi-cluster-operator.v0.11.0",
			"strimzi-cluster-operator.v0.11.1",
			"strimzi-cluster-operator.v0.12.2",
			"etcdoperator.v0.9.0",
			"prometheusoperator.0.15.0",
			"prometheusoperator.0.14.0",
			"etcdoperator.v0.6.1",
			"etcdoperator.v0.6.1",
			"etcdoperator.v0.9.0",
			"strimzi-cluster-operator.v0.12.1",
			"strimzi-cluster-operator.v0.11.0",
			"etcdoperator.v0.9.2",
			"etcdoperator.v0.9.2",
			"strimzi-cluster-operator.v0.11.1",
			"strimzi-cluster-operator.v0.11.0",
			"strimzi-cluster-operator.v0.12.1",
			"strimzi-cluster-operator.v0.11.1",
			"etcdoperator.v0.9.0",
		}

		var names []string
		var gotBundles = make([]*api.Bundle, 0)

		waitc := make(chan struct{})
		go func(t *testing.T) {
			tt := t
			for {
				in, err := stream.Recv()

				if errors.Is(err, io.EOF) {
					// read done.
					close(waitc)
					return
				}
				if err != nil {
					tt.Error(err)
					close(waitc)
					return
				}
				names = append(names, in.CsvName)
				if in.CsvName == etcdAlpha.CsvName {
					gotBundles = append(gotBundles, in)
				}
			}
		}(t)
		<-waitc

		require.ElementsMatch(expected, names, "%#v\n%#v", expected, names)

		// TODO: this test needs better expectations
		// check that one of the entries has all of the fields we expect
		checked := 0
		for _, b := range gotBundles {
			if b.CsvName != "etcdoperator.v0.9.2" {
				continue
			}
			if b.ChannelName == "stable" {
				EqualBundles(t, *etcdStable, *b)
				checked++
			}
			if b.ChannelName == "alpha" {
				EqualBundles(t, *etcdAlpha, *b)
				checked++
			}
		}
		require.Equal(2, checked)
	}
}

func EqualBundles(t *testing.T, expected, actual api.Bundle) {
	t.Helper()
	stripPlural(actual.ProvidedApis)
	stripPlural(actual.RequiredApis)

	require.ElementsMatch(t, expected.ProvidedApis, actual.ProvidedApis, "provided apis don't match: %#v\n%#v", expected.ProvidedApis, actual.ProvidedApis)
	require.ElementsMatch(t, expected.RequiredApis, actual.RequiredApis, "required apis don't match: %#v\n%#v", expected.RequiredApis, actual.RequiredApis)
	require.ElementsMatch(t, expected.Dependencies, actual.Dependencies, "dependencies don't match: %#v\n%#v", expected.Dependencies, actual.Dependencies)
	require.ElementsMatch(t, expected.Properties, actual.Properties, "properties don't match: %#v\n%#v", expected.Properties, actual.Properties)
	require.ElementsMatch(t, expected.Object, actual.Object, "objects don't match: %#v\n%#v", expected.Object, actual.Object)

	expected.RequiredApis, expected.ProvidedApis, actual.RequiredApis, actual.ProvidedApis = nil, nil, nil, nil
	expected.Dependencies, expected.Properties, actual.Dependencies, actual.Properties = nil, nil, nil, nil
	expected.Object, actual.Object = nil, nil

	opts := []cmp.Option{
		cmpopts.IgnoreUnexported(api.Bundle{}),
		cmpopts.IgnoreUnexported(api.GroupVersionKind{}),
		cmpopts.IgnoreUnexported(api.Property{}),
		cmpopts.IgnoreUnexported(api.Dependency{}),
	}

	require.Truef(t, cmp.Equal(expected, actual, opts...), cmp.Diff(expected, actual, opts...))
}

func stripPlural(gvks []*api.GroupVersionKind) {
	for i := range gvks {
		gvks[i].Plural = ""
	}
}

type includeManifests string

const (
	includeManifestsAll     includeManifests = "all"
	includeManifestsNone    includeManifests = "none"
	includeManifestsCSVOnly includeManifests = "csvOnly"
)

func etcdoperatorV0_9_2(channel string, addSkipsReplaces, addExtraProperties bool, includeManifests includeManifests) *api.Bundle {
	b := &api.Bundle{
		CsvName:     "etcdoperator.v0.9.2",
		PackageName: "etcd",
		ChannelName: channel,
		BundlePath:  "fake/etcd-operator:v0.9.2",
		Dependencies: []*api.Dependency{
			{
				Type:  "olm.gvk",
				Value: `{"group":"etcd.database.coreos.com","kind":"EtcdCluster","version":"v1beta2"}`,
			},
		},
		Properties: []*api.Property{
			{
				Type:  "olm.package",
				Value: `{"packageName":"etcd","version":"0.9.2"}`,
			},
			{
				Type:  "olm.gvk",
				Value: `{"group":"etcd.database.coreos.com","kind":"EtcdCluster","version":"v1beta2"}`,
			},
			{
				Type:  "olm.gvk",
				Value: `{"group":"etcd.database.coreos.com","kind":"EtcdRestore","version":"v1beta2"}`,
			},
			{
				Type:  "olm.gvk",
				Value: `{"group":"etcd.database.coreos.com","kind":"EtcdBackup","version":"v1beta2"}`,
			},
			{
				Type:  "olm.label",
				Value: `{"label":"testlabel"}`,
			},
			{
				Type:  "olm.label",
				Value: `{"label":"testlabel1"}`,
			},
			{
				Type:  "other",
				Value: `{"its":"notdefined"}`,
			},
		},
		ProvidedApis: []*api.GroupVersionKind{
			{Group: "etcd.database.coreos.com", Version: "v1beta2", Kind: "EtcdCluster"},
			{Group: "etcd.database.coreos.com", Version: "v1beta2", Kind: "EtcdBackup"},
			{Group: "etcd.database.coreos.com", Version: "v1beta2", Kind: "EtcdRestore"},
		},
		RequiredApis: []*api.GroupVersionKind{
			{Group: "etcd.database.coreos.com", Version: "v1beta2", Kind: "EtcdCluster"},
		},
		Version:   "0.9.2",
		SkipRange: "< 0.6.0",
	}
	if addSkipsReplaces {
		b.Replaces = "etcdoperator.v0.9.0"
		b.Skips = []string{"etcdoperator.v0.9.1"}
	}
	if addExtraProperties {
		b.Properties = append(b.Properties, []*api.Property{
			{Type: "olm.gvk.required", Value: `{"group":"etcd.database.coreos.com","kind":"EtcdCluster","version":"v1beta2"}`},
		}...)
	}
	switch includeManifests {
	case includeManifestsAll:
		b.CsvJson = "{\"apiVersion\":\"operators.coreos.com/v1alpha1\",\"kind\":\"ClusterServiceVersion\",\"metadata\":{\"annotations\":{\"alm-examples\":\"[{\\\"apiVersion\\\":\\\"etcd.database.coreos.com/v1beta2\\\",\\\"kind\\\":\\\"EtcdCluster\\\",\\\"metadata\\\":{\\\"name\\\":\\\"example\\\",\\\"namespace\\\":\\\"default\\\"},\\\"spec\\\":{\\\"size\\\":3,\\\"version\\\":\\\"3.2.13\\\"}},{\\\"apiVersion\\\":\\\"etcd.database.coreos.com/v1beta2\\\",\\\"kind\\\":\\\"EtcdRestore\\\",\\\"metadata\\\":{\\\"name\\\":\\\"example-etcd-cluster\\\"},\\\"spec\\\":{\\\"etcdCluster\\\":{\\\"name\\\":\\\"example-etcd-cluster\\\"},\\\"backupStorageType\\\":\\\"S3\\\",\\\"s3\\\":{\\\"path\\\":\\\"\\u003cfull-s3-path\\u003e\\\",\\\"awsSecret\\\":\\\"\\u003caws-secret\\u003e\\\"}}},{\\\"apiVersion\\\":\\\"etcd.database.coreos.com/v1beta2\\\",\\\"kind\\\":\\\"EtcdBackup\\\",\\\"metadata\\\":{\\\"name\\\":\\\"example-etcd-cluster-backup\\\"},\\\"spec\\\":{\\\"etcdEndpoints\\\":[\\\"\\u003cetcd-cluster-endpoints\\u003e\\\"],\\\"storageType\\\":\\\"S3\\\",\\\"s3\\\":{\\\"path\\\":\\\"\\u003cfull-s3-path\\u003e\\\",\\\"awsSecret\\\":\\\"\\u003caws-secret\\u003e\\\"}}}]\",\"olm.properties\":\"[{\\\"type\\\":\\\"other\\\",\\\"value\\\":{\\\"its\\\":\\\"notdefined\\\"}},{\\\"type\\\":\\\"olm.label\\\",\\\"value\\\":{\\\"label\\\":\\\"testlabel\\\"}},{\\\"type\\\":\\\"olm.label\\\",\\\"value\\\":{\\\"label\\\":\\\"testlabel1\\\"}}]\",\"olm.skipRange\":\"\\u003c 0.6.0\",\"tectonic-visibility\":\"ocs\"},\"name\":\"etcdoperator.v0.9.2\",\"namespace\":\"placeholder\"},\"spec\":{\"customresourcedefinitions\":{\"owned\":[{\"description\":\"Represents a cluster of etcd nodes.\",\"displayName\":\"etcd Cluster\",\"kind\":\"EtcdCluster\",\"name\":\"etcdclusters.etcd.database.coreos.com\",\"resources\":[{\"kind\":\"Service\",\"version\":\"v1\"},{\"kind\":\"Pod\",\"version\":\"v1\"}],\"specDescriptors\":[{\"description\":\"The desired number of member Pods for the etcd cluster.\",\"displayName\":\"Size\",\"path\":\"size\",\"x-descriptors\":[\"urn:alm:descriptor:com.tectonic.ui:podCount\"]},{\"description\":\"Limits describes the minimum/maximum amount of compute resources required/allowed\",\"displayName\":\"Resource Requirements\",\"path\":\"pod.resources\",\"x-descriptors\":[\"urn:alm:descriptor:com.tectonic.ui:resourceRequirements\"]}],\"statusDescriptors\":[{\"description\":\"The status of each of the member Pods for the etcd cluster.\",\"displayName\":\"Member Status\",\"path\":\"members\",\"x-descriptors\":[\"urn:alm:descriptor:com.tectonic.ui:podStatuses\"]},{\"description\":\"The service at which the running etcd cluster can be accessed.\",\"displayName\":\"Service\",\"path\":\"serviceName\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes:Service\"]},{\"description\":\"The current size of the etcd cluster.\",\"displayName\":\"Cluster Size\",\"path\":\"size\"},{\"description\":\"The current version of the etcd cluster.\",\"displayName\":\"Current Version\",\"path\":\"currentVersion\"},{\"description\":\"The target version of the etcd cluster, after upgrading.\",\"displayName\":\"Target Version\",\"path\":\"targetVersion\"},{\"description\":\"The current status of the etcd cluster.\",\"displayName\":\"Status\",\"path\":\"phase\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes.phase\"]},{\"description\":\"Explanation for the current status of the cluster.\",\"displayName\":\"Status Details\",\"path\":\"reason\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes.phase:reason\"]}],\"version\":\"v1beta2\"},{\"description\":\"Represents the intent to backup an etcd cluster.\",\"displayName\":\"etcd Backup\",\"kind\":\"EtcdBackup\",\"name\":\"etcdbackups.etcd.database.coreos.com\",\"specDescriptors\":[{\"description\":\"Specifies the endpoints of an etcd cluster.\",\"displayName\":\"etcd Endpoint(s)\",\"path\":\"etcdEndpoints\",\"x-descriptors\":[\"urn:alm:descriptor:etcd:endpoint\"]},{\"description\":\"The full AWS S3 path where the backup is saved.\",\"displayName\":\"S3 Path\",\"path\":\"s3.path\",\"x-descriptors\":[\"urn:alm:descriptor:aws:s3:path\"]},{\"description\":\"The name of the secret object that stores the AWS credential and config files.\",\"displayName\":\"AWS Secret\",\"path\":\"s3.awsSecret\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes:Secret\"]}],\"statusDescriptors\":[{\"description\":\"Indicates if the backup was successful.\",\"displayName\":\"Succeeded\",\"path\":\"succeeded\",\"x-descriptors\":[\"urn:alm:descriptor:text\"]},{\"description\":\"Indicates the reason for any backup related failures.\",\"displayName\":\"Reason\",\"path\":\"reason\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes.phase:reason\"]}],\"version\":\"v1beta2\"},{\"description\":\"Represents the intent to restore an etcd cluster from a backup.\",\"displayName\":\"etcd Restore\",\"kind\":\"EtcdRestore\",\"name\":\"etcdrestores.etcd.database.coreos.com\",\"specDescriptors\":[{\"description\":\"References the EtcdCluster which should be restored,\",\"displayName\":\"etcd Cluster\",\"path\":\"etcdCluster.name\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes:EtcdCluster\",\"urn:alm:descriptor:text\"]},{\"description\":\"The full AWS S3 path where the backup is saved.\",\"displayName\":\"S3 Path\",\"path\":\"s3.path\",\"x-descriptors\":[\"urn:alm:descriptor:aws:s3:path\"]},{\"description\":\"The name of the secret object that stores the AWS credential and config files.\",\"displayName\":\"AWS Secret\",\"path\":\"s3.awsSecret\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes:Secret\"]}],\"statusDescriptors\":[{\"description\":\"Indicates if the restore was successful.\",\"displayName\":\"Succeeded\",\"path\":\"succeeded\",\"x-descriptors\":[\"urn:alm:descriptor:text\"]},{\"description\":\"Indicates the reason for any restore related failures.\",\"displayName\":\"Reason\",\"path\":\"reason\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes.phase:reason\"]}],\"version\":\"v1beta2\"}],\"required\":[{\"description\":\"Represents a cluster of etcd nodes.\",\"displayName\":\"etcd Cluster\",\"kind\":\"EtcdCluster\",\"name\":\"etcdclusters.etcd.database.coreos.com\",\"resources\":[{\"kind\":\"Service\",\"version\":\"v1\"},{\"kind\":\"Pod\",\"version\":\"v1\"}],\"specDescriptors\":[{\"description\":\"The desired number of member Pods for the etcd cluster.\",\"displayName\":\"Size\",\"path\":\"size\",\"x-descriptors\":[\"urn:alm:descriptor:com.tectonic.ui:podCount\"]}],\"version\":\"v1beta2\"}]},\"description\":\"etcd is a distributed key value store that provides a reliable way to store data across a cluster of machines. It’s open-source and available on GitHub. etcd gracefully handles leader elections during network partitions and will tolerate machine failure, including the leader. Your applications can read and write data into etcd.\\nA simple use-case is to store database connection details or feature flags within etcd as key value pairs. These values can be watched, allowing your app to reconfigure itself when they change. Advanced uses take advantage of the consistency guarantees to implement database leader elections or do distributed locking across a cluster of workers.\\n\\n_The etcd Open Cloud Service is Public Alpha. The goal before Beta is to fully implement backup features._\\n\\n### Reading and writing to etcd\\n\\nCommunicate with etcd though its command line utility `etcdctl` or with the API using the Kubernetes Service.\\n\\n[Read the complete guide to using the etcd Open Cloud Service](https://coreos.com/tectonic/docs/latest/alm/etcd-ocs.html)\\n\\n### Supported Features\\n\\n\\n**High availability**\\n\\n\\nMultiple instances of etcd are networked together and secured. Individual failures or networking issues are transparently handled to keep your cluster up and running.\\n\\n\\n**Automated updates**\\n\\n\\nRolling out a new etcd version works like all Kubernetes rolling updates. Simply declare the desired version, and the etcd service starts a safe rolling update to the new version automatically.\\n\\n\\n**Backups included**\\n\\n\\nComing soon, the ability to schedule backups to happen on or off cluster.\\n\",\"displayName\":\"etcd\",\"icon\":[{\"base64data\":\"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\",\"mediatype\":\"image/png\"}],\"install\":{\"spec\":{\"deployments\":[{\"name\":\"etcd-operator\",\"spec\":{\"replicas\":1,\"selector\":{\"matchLabels\":{\"name\":\"etcd-operator-alm-owned\"}},\"template\":{\"metadata\":{\"labels\":{\"name\":\"etcd-operator-alm-owned\"},\"name\":\"etcd-operator-alm-owned\"},\"spec\":{\"containers\":[{\"command\":[\"etcd-operator\",\"--create-crd=false\"],\"env\":[{\"name\":\"MY_POD_NAMESPACE\",\"valueFrom\":{\"fieldRef\":{\"fieldPath\":\"metadata.namespace\"}}},{\"name\":\"MY_POD_NAME\",\"valueFrom\":{\"fieldRef\":{\"fieldPath\":\"metadata.name\"}}}],\"image\":\"quay.io/coreos/etcd-operator@sha256:c0301e4686c3ed4206e370b42de5a3bd2229b9fb4906cf85f3f30650424abec2\",\"name\":\"etcd-operator\"},{\"command\":[\"etcd-backup-operator\",\"--create-crd=false\"],\"env\":[{\"name\":\"MY_POD_NAMESPACE\",\"valueFrom\":{\"fieldRef\":{\"fieldPath\":\"metadata.namespace\"}}},{\"name\":\"MY_POD_NAME\",\"valueFrom\":{\"fieldRef\":{\"fieldPath\":\"metadata.name\"}}}],\"image\":\"quay.io/coreos/etcd-operator@sha256:c0301e4686c3ed4206e370b42de5a3bd2229b9fb4906cf85f3f30650424abec2\",\"name\":\"etcd-backup-operator\"},{\"command\":[\"etcd-restore-operator\",\"--create-crd=false\"],\"env\":[{\"name\":\"MY_POD_NAMESPACE\",\"valueFrom\":{\"fieldRef\":{\"fieldPath\":\"metadata.namespace\"}}},{\"name\":\"MY_POD_NAME\",\"valueFrom\":{\"fieldRef\":{\"fieldPath\":\"metadata.name\"}}}],\"image\":\"quay.io/coreos/etcd-operator@sha256:c0301e4686c3ed4206e370b42de5a3bd2229b9fb4906cf85f3f30650424abec2\",\"name\":\"etcd-restore-operator\"}],\"serviceAccountName\":\"etcd-operator\"}}}}],\"permissions\":[{\"rules\":[{\"apiGroups\":[\"etcd.database.coreos.com\"],\"resources\":[\"etcdclusters\",\"etcdbackups\",\"etcdrestores\"],\"verbs\":[\"*\"]},{\"apiGroups\":[\"\"],\"resources\":[\"pods\",\"services\",\"endpoints\",\"persistentvolumeclaims\",\"events\"],\"verbs\":[\"*\"]},{\"apiGroups\":[\"apps\"],\"resources\":[\"deployments\"],\"verbs\":[\"*\"]},{\"apiGroups\":[\"\"],\"resources\":[\"secrets\"],\"verbs\":[\"get\"]}],\"serviceAccountName\":\"etcd-operator\"}]},\"strategy\":\"deployment\"},\"keywords\":[\"etcd\",\"key value\",\"database\",\"coreos\",\"open source\"],\"labels\":{\"alm-owner-etcd\":\"etcdoperator\",\"operated-by\":\"etcdoperator\"},\"links\":[{\"name\":\"Blog\",\"url\":\"https://coreos.com/etcd\"},{\"name\":\"Documentation\",\"url\":\"https://coreos.com/operators/etcd/docs/latest/\"},{\"name\":\"etcd Operator Source Code\",\"url\":\"https://github.com/coreos/etcd-operator\"}],\"maintainers\":[{\"email\":\"support@coreos.com\",\"name\":\"CoreOS, Inc\"}],\"maturity\":\"alpha\",\"provider\":{\"name\":\"CoreOS, Inc\"},\"relatedImages\":[{\"image\":\"quay.io/coreos/etcd@sha256:3816b6daf9b66d6ced6f0f966314e2d4f894982c6b1493061502f8c2bf86ac84\",\"name\":\"etcd-v3.4.0\"},{\"image\":\"quay.io/coreos/etcd@sha256:49d3d4a81e0d030d3f689e7167f23e120abf955f7d08dbedf3ea246485acee9f\",\"name\":\"etcd-3.4.1\"}],\"replaces\":\"etcdoperator.v0.9.0\",\"selector\":{\"matchLabels\":{\"alm-owner-etcd\":\"etcdoperator\",\"operated-by\":\"etcdoperator\"}},\"skips\":[\"etcdoperator.v0.9.1\"],\"version\":\"0.9.2\"}}"
		b.Object = []string{
			"{\"apiVersion\":\"apiextensions.k8s.io/v1beta1\",\"kind\":\"CustomResourceDefinition\",\"metadata\":{\"name\":\"etcdbackups.etcd.database.coreos.com\"},\"spec\":{\"group\":\"etcd.database.coreos.com\",\"names\":{\"kind\":\"EtcdBackup\",\"listKind\":\"EtcdBackupList\",\"plural\":\"etcdbackups\",\"singular\":\"etcdbackup\"},\"scope\":\"Namespaced\",\"version\":\"v1beta2\"}}",
			"{\"apiVersion\":\"apiextensions.k8s.io/v1beta1\",\"kind\":\"CustomResourceDefinition\",\"metadata\":{\"name\":\"etcdclusters.etcd.database.coreos.com\"},\"spec\":{\"group\":\"etcd.database.coreos.com\",\"names\":{\"kind\":\"EtcdCluster\",\"listKind\":\"EtcdClusterList\",\"plural\":\"etcdclusters\",\"shortNames\":[\"etcdclus\",\"etcd\"],\"singular\":\"etcdcluster\"},\"scope\":\"Namespaced\",\"version\":\"v1beta2\"}}",
			b.CsvJson,
			"{\"apiVersion\":\"apiextensions.k8s.io/v1beta1\",\"kind\":\"CustomResourceDefinition\",\"metadata\":{\"name\":\"etcdrestores.etcd.database.coreos.com\"},\"spec\":{\"group\":\"etcd.database.coreos.com\",\"names\":{\"kind\":\"EtcdRestore\",\"listKind\":\"EtcdRestoreList\",\"plural\":\"etcdrestores\",\"singular\":\"etcdrestore\"},\"scope\":\"Namespaced\",\"version\":\"v1beta2\"}}",
		}
	case includeManifestsNone:
	case includeManifestsCSVOnly:
		b.CsvJson = "{\"kind\":\"ClusterServiceVersion\",\"apiVersion\":\"operators.coreos.com/v1alpha1\",\"metadata\":{\"name\":\"etcdoperator.v0.9.2\",\"creationTimestamp\":null,\"annotations\":{\"alm-examples\":\"[{\\\"apiVersion\\\":\\\"etcd.database.coreos.com/v1beta2\\\",\\\"kind\\\":\\\"EtcdCluster\\\",\\\"metadata\\\":{\\\"name\\\":\\\"example\\\",\\\"namespace\\\":\\\"default\\\"},\\\"spec\\\":{\\\"size\\\":3,\\\"version\\\":\\\"3.2.13\\\"}},{\\\"apiVersion\\\":\\\"etcd.database.coreos.com/v1beta2\\\",\\\"kind\\\":\\\"EtcdRestore\\\",\\\"metadata\\\":{\\\"name\\\":\\\"example-etcd-cluster\\\"},\\\"spec\\\":{\\\"etcdCluster\\\":{\\\"name\\\":\\\"example-etcd-cluster\\\"},\\\"backupStorageType\\\":\\\"S3\\\",\\\"s3\\\":{\\\"path\\\":\\\"\\u003cfull-s3-path\\u003e\\\",\\\"awsSecret\\\":\\\"\\u003caws-secret\\u003e\\\"}}},{\\\"apiVersion\\\":\\\"etcd.database.coreos.com/v1beta2\\\",\\\"kind\\\":\\\"EtcdBackup\\\",\\\"metadata\\\":{\\\"name\\\":\\\"example-etcd-cluster-backup\\\"},\\\"spec\\\":{\\\"etcdEndpoints\\\":[\\\"\\u003cetcd-cluster-endpoints\\u003e\\\"],\\\"storageType\\\":\\\"S3\\\",\\\"s3\\\":{\\\"path\\\":\\\"\\u003cfull-s3-path\\u003e\\\",\\\"awsSecret\\\":\\\"\\u003caws-secret\\u003e\\\"}}}]\",\"olm.properties\":\"[{\\\"type\\\":\\\"other\\\",\\\"value\\\":{\\\"its\\\":\\\"notdefined\\\"}},{\\\"type\\\":\\\"olm.label\\\",\\\"value\\\":{\\\"label\\\":\\\"testlabel\\\"}},{\\\"type\\\":\\\"olm.label\\\",\\\"value\\\":{\\\"label\\\":\\\"testlabel1\\\"}}]\",\"olm.skipRange\":\"\\u003c 0.6.0\",\"tectonic-visibility\":\"ocs\"}},\"spec\":{\"install\":{\"strategy\":\"deployment\",\"spec\":{\"deployments\":null}},\"version\":\"0.9.2\",\"maturity\":\"alpha\",\"customresourcedefinitions\":{\"owned\":[{\"name\":\"etcdclusters.etcd.database.coreos.com\",\"version\":\"v1beta2\",\"kind\":\"EtcdCluster\",\"displayName\":\"etcd Cluster\",\"description\":\"Represents a cluster of etcd nodes.\",\"resources\":[{\"name\":\"\",\"kind\":\"Service\",\"version\":\"v1\"},{\"name\":\"\",\"kind\":\"Pod\",\"version\":\"v1\"}],\"statusDescriptors\":[{\"path\":\"members\",\"displayName\":\"Member Status\",\"description\":\"The status of each of the member Pods for the etcd cluster.\",\"x-descriptors\":[\"urn:alm:descriptor:com.tectonic.ui:podStatuses\"]},{\"path\":\"serviceName\",\"displayName\":\"Service\",\"description\":\"The service at which the running etcd cluster can be accessed.\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes:Service\"]},{\"path\":\"size\",\"displayName\":\"Cluster Size\",\"description\":\"The current size of the etcd cluster.\"},{\"path\":\"currentVersion\",\"displayName\":\"Current Version\",\"description\":\"The current version of the etcd cluster.\"},{\"path\":\"targetVersion\",\"displayName\":\"Target Version\",\"description\":\"The target version of the etcd cluster, after upgrading.\"},{\"path\":\"phase\",\"displayName\":\"Status\",\"description\":\"The current status of the etcd cluster.\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes.phase\"]},{\"path\":\"reason\",\"displayName\":\"Status Details\",\"description\":\"Explanation for the current status of the cluster.\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes.phase:reason\"]}],\"specDescriptors\":[{\"path\":\"size\",\"displayName\":\"Size\",\"description\":\"The desired number of member Pods for the etcd cluster.\",\"x-descriptors\":[\"urn:alm:descriptor:com.tectonic.ui:podCount\"]},{\"path\":\"pod.resources\",\"displayName\":\"Resource Requirements\",\"description\":\"Limits describes the minimum/maximum amount of compute resources required/allowed\",\"x-descriptors\":[\"urn:alm:descriptor:com.tectonic.ui:resourceRequirements\"]}]},{\"name\":\"etcdbackups.etcd.database.coreos.com\",\"version\":\"v1beta2\",\"kind\":\"EtcdBackup\",\"displayName\":\"etcd Backup\",\"description\":\"Represents the intent to backup an etcd cluster.\",\"statusDescriptors\":[{\"path\":\"succeeded\",\"displayName\":\"Succeeded\",\"description\":\"Indicates if the backup was successful.\",\"x-descriptors\":[\"urn:alm:descriptor:text\"]},{\"path\":\"reason\",\"displayName\":\"Reason\",\"description\":\"Indicates the reason for any backup related failures.\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes.phase:reason\"]}],\"specDescriptors\":[{\"path\":\"etcdEndpoints\",\"displayName\":\"etcd Endpoint(s)\",\"description\":\"Specifies the endpoints of an etcd cluster.\",\"x-descriptors\":[\"urn:alm:descriptor:etcd:endpoint\"]},{\"path\":\"s3.path\",\"displayName\":\"S3 Path\",\"description\":\"The full AWS S3 path where the backup is saved.\",\"x-descriptors\":[\"urn:alm:descriptor:aws:s3:path\"]},{\"path\":\"s3.awsSecret\",\"displayName\":\"AWS Secret\",\"description\":\"The name of the secret object that stores the AWS credential and config files.\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes:Secret\"]}]},{\"name\":\"etcdrestores.etcd.database.coreos.com\",\"version\":\"v1beta2\",\"kind\":\"EtcdRestore\",\"displayName\":\"etcd Restore\",\"description\":\"Represents the intent to restore an etcd cluster from a backup.\",\"statusDescriptors\":[{\"path\":\"succeeded\",\"displayName\":\"Succeeded\",\"description\":\"Indicates if the restore was successful.\",\"x-descriptors\":[\"urn:alm:descriptor:text\"]},{\"path\":\"reason\",\"displayName\":\"Reason\",\"description\":\"Indicates the reason for any restore related failures.\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes.phase:reason\"]}],\"specDescriptors\":[{\"path\":\"etcdCluster.name\",\"displayName\":\"etcd Cluster\",\"description\":\"References the EtcdCluster which should be restored,\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes:EtcdCluster\",\"urn:alm:descriptor:text\"]},{\"path\":\"s3.path\",\"displayName\":\"S3 Path\",\"description\":\"The full AWS S3 path where the backup is saved.\",\"x-descriptors\":[\"urn:alm:descriptor:aws:s3:path\"]},{\"path\":\"s3.awsSecret\",\"displayName\":\"AWS Secret\",\"description\":\"The name of the secret object that stores the AWS credential and config files.\",\"x-descriptors\":[\"urn:alm:descriptor:io.kubernetes:Secret\"]}]}],\"required\":[{\"name\":\"etcdclusters.etcd.database.coreos.com\",\"version\":\"v1beta2\",\"kind\":\"EtcdCluster\",\"displayName\":\"etcd Cluster\",\"description\":\"Represents a cluster of etcd nodes.\",\"resources\":[{\"name\":\"\",\"kind\":\"Service\",\"version\":\"v1\"},{\"name\":\"\",\"kind\":\"Pod\",\"version\":\"v1\"}],\"specDescriptors\":[{\"path\":\"size\",\"displayName\":\"Size\",\"description\":\"The desired number of member Pods for the etcd cluster.\",\"x-descriptors\":[\"urn:alm:descriptor:com.tectonic.ui:podCount\"]}]}]},\"apiservicedefinitions\":{},\"displayName\":\"etcd\",\"description\":\"etcd is a distributed key value store that provides a reliable way to store data across a cluster of machines. It’s open-source and available on GitHub. etcd gracefully handles leader elections during network partitions and will tolerate machine failure, including the leader. Your applications can read and write data into etcd.\\nA simple use-case is to store database connection details or feature flags within etcd as key value pairs. These values can be watched, allowing your app to reconfigure itself when they change. Advanced uses take advantage of the consistency guarantees to implement database leader elections or do distributed locking across a cluster of workers.\\n\\n_The etcd Open Cloud Service is Public Alpha. The goal before Beta is to fully implement backup features._\\n\\n### Reading and writing to etcd\\n\\nCommunicate with etcd though its command line utility `etcdctl` or with the API using the Kubernetes Service.\\n\\n[Read the complete guide to using the etcd Open Cloud Service](https://coreos.com/tectonic/docs/latest/alm/etcd-ocs.html)\\n\\n### Supported Features\\n\\n\\n**High availability**\\n\\n\\nMultiple instances of etcd are networked together and secured. Individual failures or networking issues are transparently handled to keep your cluster up and running.\\n\\n\\n**Automated updates**\\n\\n\\nRolling out a new etcd version works like all Kubernetes rolling updates. Simply declare the desired version, and the etcd service starts a safe rolling update to the new version automatically.\\n\\n\\n**Backups included**\\n\\n\\nComing soon, the ability to schedule backups to happen on or off cluster.\\n\",\"keywords\":[\"etcd\",\"key value\",\"database\",\"coreos\",\"open source\"],\"maintainers\":[{\"name\":\"CoreOS, Inc\",\"email\":\"support@coreos.com\"}],\"provider\":{\"name\":\"CoreOS, Inc\"},\"links\":[{\"name\":\"Blog\",\"url\":\"https://coreos.com/etcd\"},{\"name\":\"Documentation\",\"url\":\"https://coreos.com/operators/etcd/docs/latest/\"},{\"name\":\"etcd Operator Source Code\",\"url\":\"https://github.com/coreos/etcd-operator\"}],\"icon\":[{\"base64data\":\"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\",\"mediatype\":\"image/png\"}],\"cleanup\":{\"enabled\":false},\"relatedImages\":[{\"name\":\"\",\"image\":\"quay.io/coreos/etcd-operator@sha256:c0301e4686c3ed4206e370b42de5a3bd2229b9fb4906cf85f3f30650424abec2\"},{\"name\":\"etcd-v3.4.0\",\"image\":\"quay.io/coreos/etcd@sha256:3816b6daf9b66d6ced6f0f966314e2d4f894982c6b1493061502f8c2bf86ac84\"},{\"name\":\"etcd-3.4.1\",\"image\":\"quay.io/coreos/etcd@sha256:49d3d4a81e0d030d3f689e7167f23e120abf955f7d08dbedf3ea246485acee9f\"}]},\"status\":{\"cleanup\":{}}}"
		b.Object = []string{b.CsvJson}
	default:
		panic(fmt.Sprintf("unexpected includeManifests value: %q", includeManifests))
	}
	return b
}

var (
	cockroachdb = &fstest.MapFile{
		Data: []byte(`{
	"defaultChannel": "stable-v6.x",
	"name": "cockroachdb",
	"schema": "olm.package"
}
{
	"entries": [
		{
		"name": "cockroachdb.v5.0.3"
		},
		{
		"name": "cockroachdb.v5.0.4",
		"replaces": "cockroachdb.v5.0.3"
		}
	],
	"name": "stable-5.x",
	"package": "cockroachdb",
	"schema": "olm.channel"
}
{
	"entries": [
		{
		"name": "cockroachdb.v6.0.0",
		"skipRange": "<6.0.0"
		}
	],
	"name": "stable-v6.x",
	"package": "cockroachdb",
	"schema": "olm.channel"
}
{
	"image": "quay.io/openshift-community-operators/cockroachdb@sha256:a5d4f4467250074216eb1ba1c36e06a3ab797d81c431427fc2aca97ecaf4e9d8",
	"name": "cockroachdb.v5.0.3",
	"package": "cockroachdb",
	"properties": [
		{
			"type": "olm.gvk",
			"value": {
				"group": "charts.operatorhub.io",
				"kind": "Cockroachdb",
				"version": "v1alpha1"
			}
		},
		{
			"type": "olm.package",
			"value": {
				"packageName": "cockroachdb",
				"version": "5.0.3"
			}
		}
	],
	"relatedImages": [
		{
			"image": "quay.io/helmoperators/cockroachdb:v5.0.3",
			"name": ""
		},
		{
			"image": "quay.io/openshift-community-operators/cockroachdb@sha256:a5d4f4467250074216eb1ba1c36e06a3ab797d81c431427fc2aca97ecaf4e9d8",
			"name": ""
		}
	],
	"schema": "olm.bundle"
}
{
	"image": "quay.io/openshift-community-operators/cockroachdb@sha256:f42337e7b85a46d83c94694638e2312e10ca16a03542399a65ba783c94a32b63",
	"name": "cockroachdb.v5.0.4",
	"package": "cockroachdb",
	"properties": [
		{
			"type": "olm.gvk",
			"value": {
				"group": "charts.operatorhub.io",
				"kind": "Cockroachdb",
				"version": "v1alpha1"
			}
		},
		{
			"type": "olm.package",
			"value": {
				"packageName": "cockroachdb",
				"version": "5.0.4"
			}
		}
	],
	"relatedImages": [
		{
			"image": "quay.io/helmoperators/cockroachdb:v5.0.4",
			"name": ""
		},
		{
			"image": "quay.io/openshift-community-operators/cockroachdb@sha256:f42337e7b85a46d83c94694638e2312e10ca16a03542399a65ba783c94a32b63",
			"name": ""
		}
	],
	"schema": "olm.bundle"
}
{
	"image": "quay.io/openshift-community-operators/cockroachdb@sha256:d3016b1507515fc7712f9c47fd9082baf9ccb070aaab58ed0ef6e5abdedde8ba",
	"name": "cockroachdb.v6.0.0",
	"package": "cockroachdb",
	"properties": [
		{
			"type": "olm.gvk",
			"value": {
				"group": "charts.operatorhub.io",
				"kind": "Cockroachdb",
				"version": "v1alpha1"
			}
		},
		{
			"type": "olm.package",
			"value": {
				"packageName": "cockroachdb",
				"version": "6.0.0"
			}
		}
	],
	"relatedImages": [
		{
			"image": "quay.io/cockroachdb/cockroach-helm-operator:6.0.0",
			"name": ""
		},
		{
			"image": "quay.io/openshift-community-operators/cockroachdb@sha256:d3016b1507515fc7712f9c47fd9082baf9ccb070aaab58ed0ef6e5abdedde8ba",
			"name": ""
		}
	],
	"schema": "olm.bundle"
}`),
	}
	deprecations = &fstest.MapFile{
		Data: []byte(`---
schema: olm.deprecations
package: cockroachdb
entries:
- reference:
    schema: olm.bundle
    name: cockroachdb.v5.0.3
  message: |
       cockroachdb.v5.0.3 is deprecated. Uninstall and install cockroachdb.v5.0.4 for support.
- reference: 
    schema: olm.package
  message: |
       package cockroachdb is end of life.  Please use 'nouveau-cockroachdb' package for support.
- reference:
    schema: olm.channel
    name: stable-5.x
  message: |
       channel stable-5.x is no longer supported.  Please switch to channel 'stable-6.x'.`),
	}

	validFS = fstest.MapFS{
		"cockroachdb.json":  cockroachdb,
		"deprecations.yaml": deprecations,
	}
)
