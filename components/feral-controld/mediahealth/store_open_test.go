package mediahealth

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

func seedStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "media-health", "store.db")
	s, err := Open(path, Options{})
	require.NoError(t, err)
	_, err = s.ApplyOutcome(Outcome{Seq: 1, Source: "https://o/work", Kind: OutcomeOK, Stage: StageReady, MediaType: "image", Attempt: 1}, true)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	return path
}

// assertFreshStore opens path (as the restart would, or as Open already
// has) and asserts it holds nothing from before and left no marker.
func assertFreshStore(t *testing.T, s *Store, path string) {
	t.Helper()
	n, err := s.Count()
	require.NoError(t, err)
	assert.Zero(t, n, "the fresh store starts empty")
	assert.NoFileExists(t, path+".opening")
}

func TestOpen_HealthyStoreKeepsItsRecordsAndLeavesNoMarker(t *testing.T) {
	path := seedStore(t)
	s, err := Open(path, Options{})
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	rec, err := s.Get("https://o/work")
	require.NoError(t, err)
	assert.NotNil(t, rec, "a store that opens and checks clean is kept")
	assert.NoFileExists(t, path+".opening")
}

// A marker left behind means the previous open died part-way (a panic or a
// fault inside bbolt): the file is deleted before it is touched again.
func TestOpen_LeftoverMarkerDiscardsTheStore(t *testing.T) {
	path := seedStore(t)
	require.NoError(t, os.WriteFile(path+".opening", nil, 0o600))

	s, err := Open(path, Options{})
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	assertFreshStore(t, s, path)
}

// The main path: tx.Check reports damage and finishes. The fixture is a real
// store with one byte flipped (found by page-corruption fuzzing) that Check
// reports as "page 9: unreachable unfreed" without faulting or looping.
func TestOpen_DamageTheCheckReportsIsReplacedByAFreshStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	gz, err := os.Open(filepath.Join("testdata", "store-unreachable-unfreed.db.gz"))
	require.NoError(t, err)
	defer func() { _ = gz.Close() }()
	r, err := gzip.NewReader(gz)
	require.NoError(t, err)
	raw, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	runaway, err := checkPages(path)
	require.False(t, runaway)
	require.ErrorContains(t, err, "integrity check", "the fixture must still read as damage to the check")

	s, err := Open(path, Options{})
	require.NoError(t, err, "damage the check reports is replaced, not left to keep media health off")
	defer func() { _ = s.Close() }()
	assertFreshStore(t, s, path)
}

func TestOpen_GarbageFileIsReplacedByAFreshStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	require.NoError(t, os.WriteFile(path, []byte("not a bbolt file, not even close"), 0o600))

	s, err := Open(path, Options{})
	require.NoError(t, err, "a file bbolt refuses is replaced, not left to keep media health off")
	defer func() { _ = s.Close() }()
	assertFreshStore(t, s, path)
}

// A check that will not stop (a page cycle in a damaged file makes tx.Check
// loop and allocate without end) deletes the file and reports runaway, so
// the daemon exits and restarts onto a fresh store. Simulated with a
// timeout no walk can meet.
func TestOpen_RunawayCheckDiscardsAndAsksForARestart(t *testing.T) {
	path := seedStore(t)
	saved := checkTimeout
	checkTimeout = time.Nanosecond
	t.Cleanup(func() { checkTimeout = saved })

	_, err := Open(path, Options{})
	require.ErrorIs(t, err, ErrStoreCheckRunaway)
	assert.NoFileExists(t, path)
	assert.NoFileExists(t, path+".opening", "the file it guarded is gone")

	checkTimeout = saved
	s, err := Open(path, Options{})
	require.NoError(t, err, "the restart opens a fresh store")
	defer func() { _ = s.Close() }()
	assertFreshStore(t, s, path)
}

// A store from a newer schema is what a rollback leaves; the newer binary
// reads it again after the next update, so it is refused and kept.
func TestOpen_NewerSchemaIsRefusedAndKept(t *testing.T) {
	path := seedStore(t)
	db, err := bolt.Open(path, 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(metaSchemaKey, []byte(strconv.Itoa(schemaVersion+1)))
	}))
	require.NoError(t, db.Close())

	_, err = Open(path, Options{})
	require.ErrorIs(t, err, errNewerSchema)
	assert.FileExists(t, path)
	assert.NoFileExists(t, path+".opening")
}
