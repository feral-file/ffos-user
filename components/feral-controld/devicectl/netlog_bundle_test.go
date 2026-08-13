package devicectl

import (
	"archive/zip"
	"bytes"
	"io"
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

// TestZipLogs_ConfiguredNetlogDirRidesBundle: netlog.dir may relocate the
// ring outside ~/.logs (e.g. somewhere OTA-durable). The uploader must archive
// the EFFECTIVE ring directory under the same netlog/ prefix, and a stale
// leftover default dir must not shadow the live ring's entries.
func TestZipLogs_ConfiguredNetlogDirRidesBundle(t *testing.T) {
	logsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, "controld.log"), []byte("daemon log"), 0o600))
	// Stale leftover from before the netlog.dir override.
	staleDir := filepath.Join(logsDir, "netlog")
	require.NoError(t, os.MkdirAll(staleDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(staleDir, "netlog-000001.jsonl"),
		[]byte(`{"kind":"note","note":"stale"}`+"\n"), 0o600))

	ringDir := t.TempDir() // outside logsDir entirely
	live := []byte(`{"kind":"outage_end"}` + "\n")
	require.NoError(t, os.WriteFile(filepath.Join(ringDir, "netlog-000001.jsonl"), live, 0o600))

	u := newTestUploader(t, logsDir, "http://unused.invalid", nil)
	u.netlogDir = ringDir

	archive, err := u.zipLogs()
	require.NoError(t, err)
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	entries := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		entries[f.Name] = data
	}
	require.Contains(t, entries, "netlog/netlog-000001.jsonl",
		"the relocated ring must ride the bundle under the netlog/ prefix")
	assert.Equal(t, live, entries["netlog/netlog-000001.jsonl"],
		"the LIVE ring's segment must win over the stale default-dir leftover")
	assert.Contains(t, entries, "controld.log")
}

// TestZipLogs_NetlogDirInsideLogsDirNotDoubleCollected: a ring relocated to a
// non-default subdirectory of the logs dir must appear only under the netlog/
// prefix — collecting it again under its own path would double-charge the
// budget and double-ship the segments.
func TestZipLogs_NetlogDirInsideLogsDirNotDoubleCollected(t *testing.T) {
	logsDir := t.TempDir()
	ringDir := filepath.Join(logsDir, "ring2")
	require.NoError(t, os.MkdirAll(ringDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(ringDir, "netlog-000001.jsonl"),
		[]byte(`{"kind":"outage_end"}`+"\n"), 0o600))

	u := newTestUploader(t, logsDir, "http://unused.invalid", nil)
	u.netlogDir = ringDir

	archive, err := u.zipLogs()
	require.NoError(t, err)
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	assert.ElementsMatch(t, []string{"netlog/netlog-000001.jsonl"}, names,
		"the ring must appear exactly once, under the netlog/ prefix")
}
