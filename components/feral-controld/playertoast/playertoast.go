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
	"time"

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
	// Show renders notice. stillCurrent (may be nil) is consulted once more
	// AFTER the manifest is read/validated and immediately before the CDP
	// send; a false return abandons the send, so a Clear during the manifest
	// read does not let an obsolete notice reach the wall
	// (feral-file/ffos-user#307).
	Show(ctx context.Context, notice sigverify.Notice, stillCurrent func() bool) error
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
func (s *sender) Show(ctx context.Context, notice sigverify.Notice, stillCurrent func() bool) error {
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
	// Bound the CDP round trip (and thus the shared c.mu hold) to the caller's
	// deadline, so a best-effort toast to a wedged player cannot monopolize
	// CDP and starve casts/status (feral-file/ffos-user#307). Default 2s when
	// the context carries no deadline.
	timeout := 2 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 {
			timeout = remaining
		} else {
			return ctx.Err()
		}
	}
	// Early out at the CDP handoff: the manifest read above can span the window
	// in which a newer valid/silent transition Clears this notice. stillCurrent
	// is also passed as the send GUARD, re-checked under CDP's write lock right
	// before the write, so a newer cast's write cannot interleave between here
	// and the toast's own write (#307).
	if stillCurrent != nil && !stillCurrent() {
		return nil
	}
	result, err := s.cdp.NoLogSendWithin(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    "window.handleCDPRequest(" + string(payload) + ")",
		"returnByValue": true,
	}, timeout, stillCurrent)
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

// Notifier is the best-effort, non-blocking toast surface commandrouter, the
// scheduler wiring, and the refresher submit to. Notify replaces any pending
// notice (latest-wins); Clear drops a pending notice — a valid or silent
// transition calls it so a stale warning never appears over newer artwork
// (feral-file/ffos-user#307). Epoch/NotifyIfEpoch fence a notice decided after
// a slow resolution (a strict refusal): the epoch is the monotonic
// display-transition token — it advances on every Notify AND Clear, and every
// locked pre-send invalidation and generation replacement Clears the toast, so
// a caller that snapshots it before resolving and passes it to NotifyIfEpoch
// enqueues only if no newer transition intervened.
type Notifier interface {
	Notify(notice sigverify.Notice)
	Clear()
	// Epoch returns the current display-transition token.
	Epoch() uint64
	// NotifyIfEpoch queues notice only if the token still equals epoch (no
	// newer transition since the snapshot); otherwise it is a no-op.
	NotifyIfEpoch(notice sigverify.Notice, epoch uint64)
	// ClearAndEpoch drops any pending notice and returns the token AFTER the
	// bump, so a replacing path can capture the epoch its own pre-send
	// invalidation created and pass it to NotifyIfEpoch after acceptance —
	// closing the window where a generation bump lands between a path's check
	// and its Notify.
	ClearAndEpoch() uint64
}

// Dispatcher serializes toasts onto a single worker with a one-slot mailbox:
// producers never block, at most one Show is ever in flight (so toasts cannot
// pile up on CDP), and a later transition supersedes a queued earlier one.
// Each send is bounded by timeout (via the Sender's context), so a wedged
// player cannot hold CDP past it.
type Dispatcher struct {
	sender  Sender
	logger  *zap.Logger
	timeout time.Duration

	mu      sync.Mutex
	pending sigverify.Notice
	has     bool
	// gen bumps on every Notify and Clear. The worker captures it at dequeue
	// and re-checks it immediately before Show, so a Clear (or newer Notify)
	// that lands after the dequeue but before the send still suppresses the
	// stale notice — Clear invalidates just-dequeued work, not only queued
	// work (feral-file/ffos-user#307). It cannot recall a send already in
	// flight; the notice's own 5s auto-dismiss bounds that, and dismissing an
	// on-screen toast would need a player dismiss contract (out of scope here).
	gen uint64

	wake chan struct{}
}

// NewDispatcher starts the worker; it stops when ctx is done.
func NewDispatcher(ctx context.Context, sender Sender, timeout time.Duration, logger *zap.Logger) *Dispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	d := &Dispatcher{sender: sender, logger: logger, timeout: timeout, wake: make(chan struct{}, 1)}
	go d.run(ctx)
	return d
}

// Notify queues notice as the pending toast, replacing any not-yet-sent one.
// Non-blocking.
func (d *Dispatcher) Notify(notice sigverify.Notice) {
	d.mu.Lock()
	d.pending = notice
	d.has = true
	d.gen++
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Epoch returns the current display-transition token (the gen counter).
func (d *Dispatcher) Epoch() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gen
}

// ClearAndEpoch drops any pending notice and returns the token after the bump.
func (d *Dispatcher) ClearAndEpoch() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.has = false
	d.gen++
	return d.gen
}

// NotifyIfEpoch queues notice only if the token still equals epoch, i.e. no
// Notify/Clear (and thus no pre-send invalidation or generation replacement)
// intervened since the caller snapshotted it. Used to fence a strict-refusal
// notice decided after a slow resolution against a newer transition that
// already replaced the artwork (feral-file/ffos-user#307).
func (d *Dispatcher) NotifyIfEpoch(notice sigverify.Notice, epoch uint64) {
	d.mu.Lock()
	if d.gen != epoch {
		d.mu.Unlock()
		return
	}
	d.pending = notice
	d.has = true
	d.gen++
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Clear drops a pending (not-yet-sent) notice. A valid or silent transition
// calls it so an earlier warning does not surface over the new artwork.
// Non-blocking; an already-dispatching send is not recalled (it is bounded).
func (d *Dispatcher) Clear() {
	d.mu.Lock()
	d.has = false
	d.gen++
	d.mu.Unlock()
}

func (d *Dispatcher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
			d.mu.Lock()
			notice, has, gen := d.pending, d.has, d.gen
			d.has = false
			d.mu.Unlock()
			if !has {
				continue
			}
			// Re-check right before the send: a Clear or newer Notify since
			// the dequeue has bumped gen, so this notice is stale and must not
			// go out (the newer Notify will wake us again with its own).
			d.mu.Lock()
			superseded := d.gen != gen
			d.mu.Unlock()
			if superseded {
				continue
			}
			sctx, cancel := context.WithTimeout(ctx, d.timeout)
			// stillCurrent lets Show abandon the send at the CDP handoff if a
			// newer transition advanced the epoch while Show read the manifest.
			if err := d.sender.Show(sctx, notice, func() bool { return d.Epoch() == gen }); err != nil {
				d.logger.Debug("player toast not shown", zap.String("notice", string(notice)), zap.Error(err))
			}
			cancel()
		}
	}
}
