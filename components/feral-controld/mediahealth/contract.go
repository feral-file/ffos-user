package mediahealth

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ContractName is the player manifest entry (contracts.mediaHealth) that
// declares both halves of the contract: the `mediaHealth` request field on
// displayPlaylist and the `outcomes` field on checkStatus.
const ContractName = "mediaHealth"

// ContractVersion is the version this daemon speaks.
const ContractVersion = 1

// PlayerContract reports whether the installed player declares the
// mediaHealth contract. Paired-rollout rule (docs/player-session-recovery.md
// §9): ff-player ships first; a manifest without the entry, or with another
// version, or that cannot be read, is "capability absent" — hints are not
// sent and outcomes are not expected — and a failed read never latches.
//
// The manifest is re-read when its mtime changes, so a player updated on
// the package rail under a running daemon is picked up on the next cast.
type PlayerContract struct {
	Path string

	mu        sync.Mutex
	checked   time.Time
	mtime     time.Time
	supported bool
}

// manifestRecheck bounds how often the file is stat'ed; a cast storm must
// not become a stat storm.
const manifestRecheck = 5 * time.Second

// Supported reports whether contracts.mediaHealth is declared at
// ContractVersion.
func (c *PlayerContract) Supported() bool {
	if c == nil || c.Path == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.checked) < manifestRecheck {
		return c.supported
	}
	c.checked = now
	info, err := os.Stat(c.Path)
	if err != nil {
		c.supported = false
		c.mtime = time.Time{}
		return false
	}
	if info.ModTime().Equal(c.mtime) {
		return c.supported
	}
	c.mtime = info.ModTime()
	c.supported = readSupport(c.Path)
	return c.supported
}

func readSupport(path string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the fixed player contract path; tests inject temp files.
	if err != nil {
		return false
	}
	var manifest struct {
		Contracts map[string]struct {
			Version int `json:"version"`
		} `json:"contracts"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false
	}
	entry, ok := manifest.Contracts[ContractName]
	return ok && entry.Version == ContractVersion
}
