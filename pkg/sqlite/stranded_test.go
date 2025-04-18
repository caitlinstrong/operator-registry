package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/operator-framework/operator-registry/pkg/image"
	"github.com/operator-framework/operator-registry/pkg/registry"
)

func TestStrandedBundleRemover(t *testing.T) {
	logrus.SetLevel(logrus.DebugLevel)
	db, cleanup := CreateTestDB(t)
	defer cleanup()
	store, err := NewSQLLiteLoader(db)
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.TODO()))

	query := NewSQLLiteQuerierFromDb(db)

	graphLoader, err := NewSQLGraphLoaderFromDB(db)
	require.NoError(t, err)

	populate := func(name string) error {
		return registry.NewDirectoryPopulator(
			store,
			graphLoader,
			query,
			map[image.Reference]string{
				image.SimpleReference("quay.io/test/" + name): "./testdata/strandedbundles/" + name,
			},
			nil).Populate(registry.ReplacesMode)
	}
	for _, name := range []string{"prometheus.0.14.0", "prometheus.0.15.0", "prometheus.0.22.2"} {
		require.NoError(t, populate(name))
	}

	// check that the bundle is orphaned
	querier := NewSQLLiteQuerierFromDb(db)
	packageBundles, err := querier.GetBundlesForPackage(context.TODO(), "prometheus")
	require.NoError(t, err)
	require.Len(t, packageBundles, 1)

	rows, err := db.QueryContext(context.TODO(), "select * from operatorbundle")
	require.NoError(t, err)
	require.Equal(t, 3, rowCount(rows))
	require.NoError(t, rows.Close())

	// check that properties are set
	rows, err = db.QueryContext(context.TODO(), `select * from properties where operatorbundle_name="prometheusoperator.0.14.0"`)
	require.NoError(t, err)
	require.True(t, rows.Next())
	require.NoError(t, rows.Close())

	// prune the orphaned bundle
	err = store.RemoveStrandedBundles()
	require.NoError(t, err)

	// other bundles in the package still exist, but the bundle is removed
	packageBundles, err = querier.GetBundlesForPackage(context.TODO(), "prometheus")
	require.NoError(t, err)
	require.Len(t, packageBundles, 1)

	rows, err = db.QueryContext(context.TODO(), "select * from operatorbundle")
	require.NoError(t, err)
	require.Equal(t, 1, rowCount(rows))
	require.NoError(t, rows.Close())

	// check that properties are removed
	rows, err = db.QueryContext(context.TODO(), `select * from properties where operatorbundle_name="prometheusoperator.0.14.0" OR operatorbundle_name="prometheusoperator.0.15.0"`)
	require.NoError(t, err)
	require.False(t, rows.Next())
	require.NoError(t, rows.Close())
}

func rowCount(rows *sql.Rows) int {
	count := 0
	for rows.Next() {
		count++
	}

	return count
}
