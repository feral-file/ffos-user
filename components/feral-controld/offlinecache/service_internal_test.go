package offlinecache

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
