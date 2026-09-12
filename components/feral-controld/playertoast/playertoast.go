// Package playertoast sends the transient signature-verification notice to
// ff-player over CDP (feral-file/ffos-user#307). It is stateless and
// best-effort: a toast never changes a cast's outcome, and it is capability-
// gated on the player's own manifest — an older bundle that predates the
// playerToast contract is degraded to "no toast", never an error the cast
// path acts on.
package playertoast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

// playerToastCommand is the CDP command ff-player dispatches on (its
// CDPRequestHandler string constant); the manifest key is the same word.
const playerToastCommand = "playerToast"

// ErrUnsupported means the connected player's manifest DECODED but does not
// carry (a usable) playerToast contract — the bundle genuinely predates the
// feature. Distinct from ErrContractUnreadable, which is transient (boot
// ordering, an OTA mid-replace of the bundle) and must be re-checked, never
// latched. Modeled on setupui.ErrPlayerContractUnreadable.
var ErrUnsupported = errors.New("player does not support playerToast")

// ErrContractUnreadable marks a read/decode failure of the manifest.
var ErrContractUnreadable = errors.New("player contract unreadable")

// Sender shows a signature-verification notice on the player. commandrouter
// holds one and calls it best-effort.
type Sender interface {
	Show(ctx context.Context, notice sigverify.Notice) error
}

// New builds a Sender that reads the player contract at manifestPath on every
// Show (the bundle can be OTA-replaced; the capability is never latched) and
// sends over cdpClient. logger may be nil.
func New(cdpClient cdp.CDP, manifestPath string, logger *zap.Logger) Sender {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &sender{cdp: cdpClient, manifestPath: manifestPath, logger: logger}
}

type sender struct {
	cdp          cdp.CDP
	manifestPath string
	logger       *zap.Logger
	// unsupportedOnce keeps the "player predates playerToast" note to one log
	// line per process: it is a fixed property of the connected bundle, not a
	// per-cast event, so logging it every cast would be noise.
	unsupportedOnce sync.Once
}

// Show validates that the connected player lists notice in its playerToast
// contract, then sends it. It returns ErrUnsupported (logged once) when the
// player predates the feature, ErrContractUnreadable on a transient manifest
// read failure, or a send/response error. Every one is best-effort to the
// caller: the cast outcome does not depend on it.
func (s *sender) Show(ctx context.Context, notice sigverify.Notice) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.cdp == nil {
		return errors.New("cdp client is required")
	}

	if err := s.validate(notice); err != nil {
		if errors.Is(err, ErrUnsupported) {
			s.unsupportedOnce.Do(func() {
				s.logger.Info("player toast unsupported by the connected player bundle; notices will not be shown")
			})
		}
		return err
	}

	payload, err := json.Marshal(map[string]any{
		"command": playerToastCommand,
		"request": map[string]any{"notice": string(notice)},
	})
	if err != nil {
		return err
	}
	result, err := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    "window.handleCDPRequest(" + string(payload) + ")",
		"returnByValue": true,
	})
	if err != nil {
		return err
	}
	return validateResult(result)
}

// validate reads the manifest and confirms it carries a version-1 playerToast
// contract whose requestKey is "request", whose acceptedResponse is {ok:true},
// and whose states list notice. A missing contract is ErrUnsupported; a
// present contract that does not list notice is a programming error (the
// notice set is closed and mirrored from the manifest), reported distinctly.
func (s *sender) validate(notice sigverify.Notice) error {
	manifest, err := readManifest(s.manifestPath)
	if err != nil {
		return err
	}
	contract, ok := manifest.Contracts[playerToastCommand]
	if !ok {
		return ErrUnsupported
	}
	if contract.Version != 1 {
		return fmt.Errorf("%w: playerToast contract version %d unsupported", ErrUnsupported, contract.Version)
	}
	if contract.RequestKey != "request" {
		return fmt.Errorf("%w: playerToast requestKey %q unsupported", ErrUnsupported, contract.RequestKey)
	}
	if !contract.AcceptedResponse.OK {
		return fmt.Errorf("%w: playerToast acceptedResponse.ok is false", ErrUnsupported)
	}
	for _, st := range contract.States {
		if st == string(notice) {
			return nil
		}
	}
	return fmt.Errorf("playerToast notice %q not listed in the player contract", notice)
}

type manifest struct {
	Contracts map[string]toastContract `json:"contracts"`
}

type toastContract struct {
	Version          int              `json:"version"`
	RequestKey       string           `json:"requestKey"`
	States           []string         `json:"states"`
	AcceptedResponse acceptedResponse `json:"acceptedResponse"`
}

type acceptedResponse struct {
	OK bool `json:"ok"`
}

func readManifest(path string) (manifest, error) {
	if path == "" {
		return manifest{}, fmt.Errorf("%w: player contract path is empty", ErrContractUnreadable)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Production uses the fixed player contract path; tests inject temp files.
	if err != nil {
		return manifest{}, fmt.Errorf("%w: %w", ErrContractUnreadable, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifest{}, fmt.Errorf("%w: decode player contract: %w", ErrContractUnreadable, err)
	}
	return m, nil
}

// validateResult confirms the player accepted the toast ({ok:true}), peeling
// the CDP Runtime.evaluate envelope the same way the mint-pairing display
// does (the cdp client's post-processed shape plus the raw fallback).
func validateResult(result any) error {
	response, err := normalizeEvaluationResult(result)
	if err != nil {
		return err
	}
	ok, hasOK := response["ok"].(bool)
	if !hasOK {
		return fmt.Errorf("player toast response missing ok: %v", response)
	}
	if !ok {
		return fmt.Errorf("player toast rejected request: %v", response)
	}
	return nil
}

func normalizeEvaluationResult(result any) (map[string]any, error) {
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("player toast returned unsupported result type %T", result)
	}
	if _, hasException := resultMap["exceptionDetails"]; hasException {
		return nil, fmt.Errorf("player toast evaluation raised exception: %v", resultMap["exceptionDetails"])
	}
	if _, hasOK := resultMap["ok"]; hasOK {
		return resultMap, nil
	}
	if message, ok := resultMap["message"]; ok {
		return normalizeEvaluationResult(message)
	}
	if value, ok := resultMap["value"]; ok {
		if raw, ok := value.(string); ok {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				return nil, fmt.Errorf("decode player toast response: %w", err)
			}
			return decoded, nil
		}
		return normalizeEvaluationResult(value)
	}
	rawResult, hasResult := resultMap["result"]
	if !hasResult {
		return resultMap, nil
	}
	rawResultMap, ok := rawResult.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("player toast returned malformed Runtime.evaluate result: %v", resultMap)
	}
	if value, ok := rawResultMap["value"]; ok {
		if raw, ok := value.(string); ok {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				return nil, fmt.Errorf("decode player toast response: %w", err)
			}
			return decoded, nil
		}
		return normalizeEvaluationResult(value)
	}
	return nil, fmt.Errorf("player toast returned unsupported Runtime.evaluate result: %v", rawResultMap)
}
