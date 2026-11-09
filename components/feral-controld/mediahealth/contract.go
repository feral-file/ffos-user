package mediahealth

import (
	"encoding/json"
	"os"
	"slices"
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

	mu      sync.Mutex
	checked time.Time
	mtime   time.Time
	support support
}

// support is what one manifest read established.
type support struct {
	// declared: contracts.mediaHealth at ContractVersion.
	declared bool
	// push: the entry also lists HintPushCommand in its commands, so the
	// hint-only push may be sent (feature-detected by name, the version
	// stays 1: controld gates the whole contract on version == 1, and a
	// bump would switch hints off on every fielded daemon).
	push bool
}

// HintPushCommand is the player command that replaces the held hint map
// for the cast on the wall without a playlist push (#392 follow-up).
const HintPushCommand = "setMediaHealth"

// manifestRecheck bounds how often the file is stat'ed; a cast storm must
// not become a stat storm.
const manifestRecheck = 5 * time.Second

// Supported reports whether contracts.mediaHealth is declared at
// ContractVersion.
func (c *PlayerContract) Supported() bool {
	return c.read().declared
}

// SupportsHintPush reports whether the declared contract also lists the
// hint-only push command.
func (c *PlayerContract) SupportsHintPush() bool {
	sup := c.read()
	return sup.declared && sup.push
}

func (c *PlayerContract) read() support {
	if c == nil || c.Path == "" {
		return support{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.checked) < manifestRecheck {
		return c.support
	}
	c.checked = now
	info, err := os.Stat(c.Path)
	if err != nil {
		c.support = support{}
		c.mtime = time.Time{}
		return c.support
	}
	if info.ModTime().Equal(c.mtime) {
		return c.support
	}
	c.mtime = info.ModTime()
	c.support = readSupport(c.Path)
	return c.support
}

func readSupport(path string) support {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the fixed player contract path; tests inject temp files.
	if err != nil {
		return support{}
	}
	var manifest struct {
		Contracts map[string]struct {
			Version  int      `json:"version"`
			Commands []string `json:"commands"`
		} `json:"contracts"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return support{}
	}
	entry, ok := manifest.Contracts[ContractName]
	if !ok || entry.Version != ContractVersion {
		return support{}
	}
	return support{declared: true, push: slices.Contains(entry.Commands, HintPushCommand)}
}
