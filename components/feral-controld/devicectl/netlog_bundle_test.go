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
