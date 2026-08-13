package devicectl

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestZipLogs_IncludesNetlogRing pins the stage-2a free ride (plan branch
// B1): the recorder's ring lives under ~/.logs, so uploadLogs bundles its
// segments with zero uploader change. If the walk ever stops recursing, this
// is the test that names the casualty.
// TestZipLogs_NetlogRingSurvivesBudgetExhaustion: the ring gets budget
// priority. Without it, the walk consumed the shared 128 MiB budget in
// directory order — and "backup" (7 days of rotated dailies, ballooning on
// exactly the flapping devices this feature targets) sorts before "netlog",
// so the outage artifact could be evicted from the very bundle raised to
// diagnose the outage.
func TestZipLogs_NetlogRingSurvivesBudgetExhaustion(t *testing.T) {
	logsDir := t.TempDir()
	backupDir := filepath.Join(logsDir, "backup")
	require.NoError(t, os.MkdirAll(backupDir, 0o750))
	// 90 bytes: with a 100-byte budget consumed walk-first, the ring's
	// 30-byte segment would not fit afterwards.
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, "controld_2026-08-12.log"),
		bytes.Repeat([]byte("x"), 90), 0o600))
	netlogDir := filepath.Join(logsDir, "netlog")
	require.NoError(t, os.MkdirAll(netlogDir, 0o750))
	seg := []byte(`{"kind":"outage_end"}` + "\n") // 22 bytes
	require.NoError(t, os.WriteFile(filepath.Join(netlogDir, "netlog-000001.jsonl"), seg, 0o600))

	u := newTestUploader(t, logsDir, "http://unused.invalid", nil)
	u.maxInputBytes = 100

	archive, err := u.zipLogs()
	require.NoError(t, err)
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	assert.Contains(t, names, "netlog/netlog-000001.jsonl",
		"the ring must survive budget exhaustion by the rest of the tree")
	assert.NotContains(t, names, "backup/controld_2026-08-12.log",
		"the oversized backup is what gets skipped, not the ring")
}

func TestZipLogs_IncludesNetlogRing(t *testing.T) {
	logsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, "controld.log"), []byte("daemon log"), 0o600))
	netlogDir := filepath.Join(logsDir, "netlog")
	require.NoError(t, os.MkdirAll(netlogDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(netlogDir, "netlog-000001.jsonl"),
		[]byte(`{"kind":"internet"}`+"\n"), 0o600))

	u := newTestUploader(t, logsDir, "http://unused.invalid", nil)
	archive, err := u.zipLogs()
	require.NoError(t, err)

	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	assert.ElementsMatch(t, []string{"controld.log", "netlog/netlog-000001.jsonl"}, names,
		"netlog segments must ride the bundle")
}
