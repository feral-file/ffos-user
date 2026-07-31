package offlinecache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// truncateReason is exercised end-to-end through Status in
// service_test.go; these cover the boundaries that are awkward to reach
// through a real capture record.
func TestTruncateReason(t *testing.T) {
	t.Run("short reason is untouched", func(t *testing.T) {
		reason := "csp_blocked; fetch_failed:https://example.com/a.js"
		assert.Equal(t, reason, truncateReason(reason))
	})

	t.Run("reason exactly at the budget is untouched", func(t *testing.T) {
		reason := strings.Repeat("x", maxReasonBytes)
		assert.Equal(t, reason, truncateReason(reason))
	})

	t.Run("keeps whole entries and counts the dropped ones", func(t *testing.T) {
		entries := make([]string, 40)
		for i := range entries {
			entries[i] = fmt.Sprintf("fetch_failed:https://example.com/asset-%02d.js", i)
		}
		got := truncateReason(strings.Join(entries, reasonSeparator))

		// Read the dropped count off the marker rather than hard-coding
		// how many entries happen to fit: what matters is that kept +
		// dropped accounts for every entry.
		marker := got[strings.LastIndex(got, "…"):]
		var dropped int
		_, err := fmt.Sscanf(marker, "…(+%d more)", &dropped)
		require.NoError(t, err)
		kept := strings.Split(got[:strings.LastIndex(got, reasonSeparator)], reasonSeparator)
		assert.Equal(t, len(entries), len(kept)+dropped)

		for _, entry := range kept {
			assert.Contains(t, entries, entry, "kept entries must not be cut mid-token")
		}
		assert.LessOrEqual(t, len(got), maxReasonBytes+len(reasonSeparator)+len(droppedReasonMarker(dropped)))
	})

	t.Run("a single oversized entry is cut on a rune boundary", func(t *testing.T) {
		// Multi-byte runes packed so the budget lands mid-rune.
		entry := "fetch_failed:https://example.com/" + strings.Repeat("é", maxReasonBytes)
		got := truncateReason(entry)

		assert.True(t, utf8.ValidString(got), "truncation must not split a rune")
		assert.True(t, strings.HasSuffix(got, "…"))
		assert.LessOrEqual(t, len(got), maxReasonBytes+len("…"))
	})

	t.Run("a single oversized entry still reports the entries after it", func(t *testing.T) {
		first := "fetch_failed:https://example.com/" + strings.Repeat("a", maxReasonBytes)
		got := truncateReason(strings.Join([]string{first, "csp_blocked", "csp_blocked"}, reasonSeparator))

		assert.True(t, utf8.ValidString(got))
		assert.Contains(t, got, droppedReasonMarker(2))
	})
}

// transientFailReadOS delegates to a real OS wrapper but fails ReadFile
// for one specific path — the same transient-EIO shape
// store_test.go's failReadOS simulates, duplicated here because this
// internal test package cannot import the external one.
type transientFailReadOS struct {
	wrapper.OS
	failPath string
}

func (f *transientFailReadOS) ReadFile(path string) ([]byte, error) {
	if path == f.failPath {
		return nil, fmt.Errorf("simulated transient read error for %s", path)
	}
	return f.OS.ReadFile(path)
}

// TestService_EvictDownTo_StopsAfterOneVictimWhenGCFails pins the
// eviction loop's behavior at the GC seam (reviewer finding on the
// GC-abort change): when a transiently unreadable record makes gc()
// abort, the loop has ALREADY deleted one healthy victim's record before
// discovering the failure — DeleteItem removes only the JSON, so those
// bytes are not reclaimed until a later successful GC — and must then
// stop rather than keep burning healthy records for zero freed bytes.
// This is the accepted cost of GC's abort-on-transient-error direction:
// one record per eviction attempt, bounded, recoverable (the record's
// blobs are swept once GC succeeds again), versus a sweep over an
// incomplete keep-set destroying referenced blobs unrecoverably.
func TestService_EvictDownTo_StopsAfterOneVictimWhenGCFails(t *testing.T) {
	root := t.TempDir()
	failOS := &transientFailReadOS{OS: wrapper.NewOS()}
	logger := zaptest.NewLogger(t)
	store := NewStore(root, failOS, wrapper.NewJSON(), logger)

	seed := func(id, content string, capturedAt time.Time) {
		blobHash, err := store.WriteBlob(strings.NewReader(content), 0)
		require.NoError(t, err)
		require.NoError(t, store.SaveItem(&ItemRecord{
			Item:       dp1playlist.PlaylistItem{ID: id, Source: "https://example.com/" + id},
			Entry:      "https://example.com/" + id,
			Resources:  []Resource{{URL: "https://example.com/" + id, Status: 200, SHA256: blobHash}},
			Coverage:   Coverage{Complete: true},
			CapturedAt: capturedAt,
		}))
	}
	seed("item-victim", "victim payload", time.Now().Add(-2*time.Hour))
	seed("item-survivor", "survivor payload", time.Now().Add(-1*time.Hour))
	seed("item-flaky", "flaky payload", time.Now())

	svc, ok := NewService(store, nil, nil, nil, wrapper.NewJSON(), 5000, 1, nil, AdmissionOptions{}, logger).(*service)
	require.True(t, ok)

	// The flaky record reads fine during the victim scan setup above;
	// from here on it fails, so oldestEvictableItem skips it (its own
	// LoadItem errors) and gc()'s mark phase aborts on it.
	failOS.failPath = filepath.Join(root, "items", SourceKey("https://example.com/item-flaky")+".json")

	// Target 0 with three items on disk: without the gc-failure stop this
	// loop would evict every readable record trying to get under target.
	svc.evictDownTo(0, "", "", false)

	_, err := store.LoadItem(SourceKey("https://example.com/item-victim"))
	assert.ErrorIs(t, err, ErrItemNotFound, "the oldest readable record is deleted before the GC failure is discovered")
	_, err = store.LoadItem(SourceKey("https://example.com/item-survivor"))
	assert.NoError(t, err, "the loop must stop after the failed GC instead of burning further healthy records")
	for _, content := range []string{"victim payload", "survivor payload", "flaky payload"} {
		sum := sha256.Sum256([]byte(content))
		_, err := store.ReadBlob(hex.EncodeToString(sum[:]))
		assert.NoError(t, err, "no blob bytes may be reclaimed while GC is aborting")
	}
}
