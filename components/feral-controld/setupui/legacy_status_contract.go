// Transitional legacy boot-recovery fuse. Remove with devicectl boot recovery
// and media-health manifest consumers; native narration never reads this file.
package setupui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const DefaultContractPath = "/opt/feral/feral-player/ffos-player-contract.json"

type playerContractManifest struct {
	Contracts map[string]playerDisplayContract `json:"contracts"`
}

type playerDisplayContract struct {
	Version          int                            `json:"version"`
	RequestKey       string                         `json:"requestKey"`
	States           []string                       `json:"states"`
	AcceptedResponse playerContractAcceptedResponse `json:"acceptedResponse"`
}

type playerContractAcceptedResponse struct {
	OK bool `json:"ok"`
}

// ErrPlayerContractUnreadable marks a validation failure caused by failing to
// READ or DECODE the player contract manifest, as opposed to a successfully
// decoded manifest that lacks the contract being checked. The two must not be
// conflated: unreadable may be transient (boot ordering, an OTA mid-replace
// of the player bundle — a torn write is byte-garbage, not evidence) and
// must be re-checked on the next attempt, never latched; a manifest that
// DECODED but lacks the contract means the connected player's build
// genuinely does not support it. Exported so
// other packages' capability fuses can apply the identical distinction —
// devicectl's boot-recovery classification (design doc §5) checks
// errors.Is(err, setupui.ErrPlayerContractUnreadable) against
// ValidatePlayerStatusContract exactly as this package's own
// narrationSupported does against validateSetupDisplayContract.
var ErrPlayerContractUnreadable = errors.New("player contract unreadable")

// readPlayerContractManifest reads and decodes the player contract manifest
// at path, wrapping BOTH read and decode failures in
// ErrPlayerContractUnreadable: undecodable bytes are a torn or partial write
// (an OTA mid-replace of the bundle), which proves nothing about the player
// — no shipped build has ever carried invalid JSON, so treating it as
// "genuinely lacks the contract" would latch capability fuses off a
// transient state (narrationSupported once killed ALL narration for the
// process lifetime this way). Genuine absence requires a manifest that
// DECODED and lacks the contract key. Shared by every contract-specific
// validator in this file (validateSetupDisplayContract,
// ValidatePlayerStatusContract) so the unreadable-vs-absent distinction is
// defined exactly once.
func readPlayerContractManifest(path string) (playerContractManifest, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return playerContractManifest{}, fmt.Errorf("player contract path is empty")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Production uses the fixed player contract path; tests inject temp files.
	if err != nil {
		return playerContractManifest{}, fmt.Errorf("%w: %w", ErrPlayerContractUnreadable, err)
	}
	var manifest playerContractManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return playerContractManifest{}, fmt.Errorf("%w: decode player contract: %w", ErrPlayerContractUnreadable, err)
	}
	return manifest, nil
}

// PlayerStatusContractVersion is the version ValidatePlayerStatusContract
// requires for contracts.playerStatus (design doc §5.3).
const PlayerStatusContractVersion = 1

// ValidatePlayerStatusContract reports whether the player manifest at path
// advertises contracts.playerStatus {version:1} — the structured status probe
// (window.__ffosPlayerStatus) devicectl's boot-recovery classification and
// the NavigateHome error-page/route gates depend on. Sibling of
// validateSetupDisplayContract, sharing its exact unreadable-vs-absent
// distinction via ErrPlayerContractUnreadable. Exported here — rather than in
// a new shared package — because devicectl already imports this package for
// narration, so it can call this directly with no new import-cycle risk:
// setupui imports nothing back from devicectl.
func ValidatePlayerStatusContract(path string) error {
	manifest, err := readPlayerContractManifest(path)
	if err != nil {
		return err
	}
	contract, ok := manifest.Contracts["playerStatus"]
	if !ok {
		return fmt.Errorf("missing contracts.playerStatus")
	}
	if contract.Version != PlayerStatusContractVersion {
		return fmt.Errorf("contracts.playerStatus.version must be %d", PlayerStatusContractVersion)
	}
	return nil
}
