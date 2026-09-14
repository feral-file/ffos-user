// Package playertoast sends the transient signature-verification notice to
// ff-player over CDP (feral-file/ffos-user#307). It is stateless and
// best-effort: a toast never changes a cast's outcome, and it is capability-
// gated on the player's own manifest — an older bundle that predates the
// playerToast contract is degraded to "no toast", never an error the cast
// path acts on.
//
// Transport: every toast rides its OWN short-lived CDP session to the kiosk
// page (dial, one Runtime.evaluate, close), never the daemon's shared
// synchronous cdp.CDP client. That client serializes one write+read behind a
// single mutex with a socket-level deadline, so a toast on it could only ever
// (a) hold the cast/status path for its deadline, (b) tear the shared session
// down on its own timeout, or (c) leave a poisoned socket behind (gorilla
// deadlines are sticky) for the next cast to trip on — one of the three for
// every choice of deadline and recovery policy. An isolated session has none
// of those coupling points: a slow or wedged player costs the toast its own
// 2s and nothing else. Same precedent as offline-cache replay and the
// User-Agent rewrite, which attach their own sessions beside the shared
// client (Chromium DevTools accepts several clients per page target). The
// per-toast dial is a loopback /json fetch plus a websocket handshake, once
// per non-valid cast — noise next to the cast itself.
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

// defaultTimeout bounds one toast end to end (dial plus evaluate) when the
// caller's context carries no deadline of its own. Short on purpose: a
// best-effort notice must give up long before anyone could mistake it for a
// stalled cast, and its session is its own, so nothing else waits on it.
const defaultTimeout = 2 * time.Second

// ErrUnsupported means the connected player's manifest DECODED but does not
// carry (a usable) playerToast contract — the bundle genuinely predates the
// feature. Distinct from ErrContractUnreadable, which is transient (boot
// ordering, an OTA mid-replace of the bundle) and must be re-checked, never
// latched. Modeled on setupui.ErrPlayerContractUnreadable.
var ErrUnsupported = errors.New("player does not support playerToast")

// ErrContractUnreadable marks a read/decode failure of the manifest.
var ErrContractUnreadable = errors.New("player contract unreadable")

// Session is the one-shot CDP session a toast rides on. Owned here (the
// consumer) so this package does not import the offline-cache package that
// implements it: offlinecache.CDPSession satisfies it and main wires the dial.
type Session interface {
	// Send issues one CDP command and returns the JSON-RPC result member;
	// the reply wait is bounded by ctx.
	Send(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error)
	// Close tears the session down. Idempotent.
	Close() error
}

// Dialer opens a fresh Session to the kiosk page, bounded by ctx.
type Dialer func(ctx context.Context) (Session, error)

// Sender shows a signature-verification notice on the player. commandrouter
// holds one and calls it best-effort.
type Sender interface {
	// Show renders notice. stillCurrent (may be nil) is consulted again
	// AFTER the manifest is read/validated and AFTER the dial, immediately
	// before the evaluate; a false return abandons the send, so a Clear
	// during either wait does not let an obsolete notice reach the wall
	// (feral-file/ffos-user#307).
	Show(ctx context.Context, notice sigverify.Notice, stillCurrent func() bool) error
}

// New builds a Sender that reads the player contract at manifestPath on every
// Show (the bundle can be OTA-replaced; the capability is never latched) and
// sends each notice over a session it dials for that notice alone. logger may
// be nil.
func New(dial Dialer, manifestPath string, logger *zap.Logger) Sender {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &sender{dial: dial, manifestPath: manifestPath, logger: logger}
}

type sender struct {
	dial         Dialer
	manifestPath string
	logger       *zap.Logger
	// unsupportedOnce keeps the "player predates playerToast" note to one log
	// line per process: it is a fixed property of the connected bundle, not a
	// per-cast event, so logging it every cast would be noise.
	unsupportedOnce sync.Once
}

// Show validates that the connected player lists notice in its playerToast
// contract, then dials a session and sends it. It returns ErrUnsupported
// (logged once) when the player predates the feature, ErrContractUnreadable
// on a transient manifest read failure, or a dial/send/response error. Every
// one is best-effort to the caller: the cast outcome does not depend on it.
func (s *sender) Show(ctx context.Context, notice sigverify.Notice, stillCurrent func() bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.dial == nil {
		return errors.New("session dialer is required")
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
	// Bound dial and evaluate together. The dialer's own ceiling exists for
	// capture/replay attach and is far too generous for a notice; WithTimeout
	// keeps the sooner deadline, so a caller's tighter one still wins.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	// Early out before dialing: the manifest read above can span the window
	// in which a newer valid/silent transition Clears this notice.
	if stillCurrent != nil && !stillCurrent() {
		return nil
	}
	session, err := s.dial(ctx)
	if err != nil {
		return fmt.Errorf("dial player session: %w", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			s.logger.Debug("player toast session close", zap.Error(err))
		}
	}()
	// Re-check after the dial, the longest step. The toast has its own
	// socket, so nothing serializes it against a cast's write on the shared
	// client; this check narrows the stale-notice window to the evaluate
	// itself (microseconds), and the notice self-dismisses in 5s regardless.
	// Accepted trade-off for never coupling the toast to the cast socket.
	if stillCurrent != nil && !stillCurrent() {
		return nil
	}
	raw, err := session.Send(ctx, cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    "window.handleCDPRequest(" + string(payload) + ")",
		"returnByValue": true,
	})
	if err != nil {
		return fmt.Errorf("send player toast: %w", err)
	}
	return validateRaw(raw)
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

// validateRaw decodes the raw Runtime.evaluate result member the session
// hands back ({"result":{"type","value"},"exceptionDetails"?}) and confirms
// the player accepted the toast.
func validateRaw(raw json.RawMessage) error {
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("decode player toast reply: %w", err)
	}
	return validateResult(result)
}

// validateResult confirms the player accepted the toast ({ok:true}), peeling
// the Runtime.evaluate envelope down to handleCDPRequest's {message:{ok}}
// the same way the mint-pairing display does.
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
// player cannot hold the toast's own session past it.
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
