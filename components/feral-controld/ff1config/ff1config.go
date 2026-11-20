// Package ff1config reads the running build descriptor written into the FF1
// image (constants.FF1_CONFIG_FILE): the installed version, the distribution
// branch and the update endpoint. Every reader in controld goes through here,
// so the OTA gate, device status, the LAN status payload, the claim QR and the
// setup screen can never disagree about which build is running.
package ff1config

import (
	"encoding/json"
	"fmt"
	"os"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
)

// FileReader is the slice of wrapper.OS a reader needs.
type FileReader interface {
	ReadFile(path string) ([]byte, error)
}

// Decoder is the slice of wrapper.JSON a reader needs.
type Decoder interface {
	Unmarshal(data []byte, v any) error
}

// Build is the installed build as the descriptor states it. Values are
// returned as written; callers that display them trim their own.
type Build struct {
	Version  string
	Branch   string
	Endpoint string
}

// Read reads and parses the descriptor through the caller's seams, so a
// package that mocks its OS or JSON wrapper keeps doing so.
func Read(fs FileReader, dec Decoder) (Build, error) {
	data, err := fs.ReadFile(constants.FF1_CONFIG_FILE)
	if err != nil {
		return Build{}, fmt.Errorf("reading FF1 config: %w", err)
	}
	var cfg struct {
		Version  string `json:"version"`
		Branch   string `json:"branch"`
		Endpoint string `json:"endpoint"`
	}
	if err := dec.Unmarshal(data, &cfg); err != nil {
		return Build{}, fmt.Errorf("parsing FF1 config: %w", err)
	}
	return Build{Version: cfg.Version, Branch: cfg.Branch, Endpoint: cfg.Endpoint}, nil
}

// ReadInstalled is Read against the real filesystem, for callers with no
// injected seams.
func ReadInstalled() (Build, error) {
	return Read(osReader{}, stdJSON{})
}

type osReader struct{}

func (osReader) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) } //nolint:gosec // Read passes only the fixed FF1 config path.

type stdJSON struct{}

func (stdJSON) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
