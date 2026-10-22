package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// Public error codes (§4.4): sent in clear because no reply key can be
// trusted.
const (
	CodeBadEnvelope        = "bad_envelope"
	CodeUnknownController  = "unknown_controller"
	CodeBadSignature       = "bad_signature"
	CodeEnrollmentRequired = "enrollment_required"
	CodeEnvelopeRequired   = "envelope_required"
	CodeLocked             = "locked"
	// CodeInvalidMessage refuses a plaintext message whose top level is
	// ambiguous (duplicate or case-variant member names, §7.2).
	CodeInvalidMessage = "invalid_message"
	// CodeScreenProtected refuses /api/screenshot while the screen may show
	// a live invitation (§7.2).
	CodeScreenProtected = "screen_protected"
	// CodeBusy: too many envelopes are being opened at once; retry.
	CodeBusy = "busy"
)

// Encrypted reply codes (§4.3).
const (
	CodeInvalidRequest    = "invalid_request"
	CodeExpired           = "expired"
	CodeReplay            = "replay"
	CodePermissionDenied  = "permission_denied"
	CodeAccessRevoked     = "access_revoked"
	CodeInvitationInvalid = "invitation_invalid"
	CodePending           = "pending"
	CodeNotApproved       = "not_approved"
	CodeRateLimited       = "rate_limited"
	CodeCommandFailed     = "command_failed"
	CodeInternalError     = "internal_error"
)

// Ceremony limits (§5).
const (
	invitationTTL = 10 * 60
	pendingTTL    = 5 * 60
	// pendingMaxLife caps a pending request's whole life, refreshes
	// included, so one entry cannot hold a slot forever (review N3).
	pendingMaxLife     = 10 * 60
	maxPending         = 4
	pendingNewInterval = 10
	// pendingRefreshInterval: a pending ctl may refresh at most this often.
	pendingRefreshInterval = 10
	// maxPendingPerLANRemote is the per-LAN-host cap (§5.2) under the
	// global maxPending.
	maxPendingPerLANRemote = 1
	approvalCodePrefix     = "ff1-approval-code/v1|"
	// NotificationApprovalRequest is the §5.2 sealed notification type.
	NotificationApprovalRequest = "controller_approval_request"
)

// State is the device control state (§7.2).
type State string

const (
	StateLegacy   State = "legacy"
	StateEnrolled State = "enrolled"
	StateEnforced State = "enforced"
	StateLocked   State = "locked"
)

// PublicError is a §3.4 step 1-3 failure (or a locked device): the caller
// sends Body() in clear with HTTPStatus() on the LAN, or Body() as the RPC
// message on the relayer.
type PublicError struct {
	Code string
	Err  error
}

func (e *PublicError) Error() string { return "control: " + e.Code + ": " + fmt.Sprint(e.Err) }
func (e *PublicError) Unwrap() error { return e.Err }

// Body is {"error":{"code":"<code>"}}. The cause is never included: it
// would turn the device into a padding/validity oracle for whoever is on
// the path.
func (e *PublicError) Body() []byte { return PublicErrorBody(e.Code) }

// HTTPStatus is the LAN status for the code (§4.4).
func (e *PublicError) HTTPStatus() int { return PublicErrorStatus(e.Code) }

// publicErrorMessages are the hints an out-of-date app or ff-cli prints for
// a plaintext refusal it has no code for (§4.4). Clients branch on code only.
var publicErrorMessages = map[string]string{
	CodeEnrollmentRequired: "this FF1 needs signed requests: update the Feral File app or ff-cli",
	CodeEnvelopeRequired:   "this FF1 needs signed requests: update the Feral File app or ff-cli",
}

// PublicErrorBody renders a public error body; also used by Policy denials.
func PublicErrorBody(code string) []byte {
	if msg, ok := publicErrorMessages[code]; ok {
		return []byte(`{"error":{"code":` + jsonString(code) + `,"message":` + jsonString(msg) + `}}`)
	}
	return []byte(`{"error":{"code":` + jsonString(code) + `}}`)
}

// PublicErrorStatus maps a public code to its LAN HTTP status.
func PublicErrorStatus(code string) int {
	switch code {
	case CodeEnvelopeRequired, CodeEnrollmentRequired, CodeScreenProtected:
		return http.StatusForbidden
	case CodeInvalidMessage:
		return http.StatusBadRequest
	case CodeBusy:
		return http.StatusServiceUnavailable
	}
	return http.StatusUnauthorized
}

func publicErr(code string, err error) *PublicError { return &PublicError{Code: code, Err: err} }

// Principal is who an admitted request acts as. It is the only authority
// signal downstream code may use; the transport a request arrived on is not.
type Principal struct {
	ControllerID string `json:"controllerId"`
	Role         Role   `json:"role"`
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p (M2 threads it through Process).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal on ctx, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Sealed is one §9 sealed notification, the "message" of the wire object.
type Sealed struct {
	V   int    `json:"v"`
	Ctl string `json:"ctl"`
	JWE string `json:"jwe"`
}

// Wire renders the §9 carriage object for the LAN WebSocket and the relayer
// notification channel.
func (s Sealed) Wire() ([]byte, error) { return s.WireWith(0, "") }

// WireWith renders the §9 carriage object with the given
// persist_record_count and the ORIGINAL notification's messageID (review
// L8): both are routing metadata (a count, and an opaque random id the
// device minted), never content. controlgate passes 0: sealed copies are
// not persisted (§9). messageID is omitted when empty.
func (s Sealed) WireWith(persistRecordCount int, messageID string) ([]byte, error) {
	return marshalJSON(struct {
		Type               string `json:"type"`
		MessageID          string `json:"messageID,omitempty"`
		NotificationType   string `json:"notification_type"`
		PersistRecordCount int    `json:"persist_record_count"`
		Message            Sealed `json:"message"`
	}{"notification", messageID, "sealed", persistRecordCount, s})
}

// Notice is a notification the authority produced, before sealing: the
// caller seals one copy per channel (§9) with Authority.Seal.
type Notice struct {
	Ctl  string
	Type string
	Body any
}

// Admission is the outcome of opening an authentic request.
//
// If Final is non-empty the authority has already produced the complete
// reply — an encrypted error, or the result of a control-plane operation it
// executes itself (enroll, requestAccess, approve/revoke/list/registerController) —
// and the caller sends Final and executes nothing. Otherwise the request is
// an admitted op=cmd: its replay window is already durable, the caller runs
// Command/Request as Principal and answers with Reply.
type Admission struct {
	Op        Op
	Command   string
	Request   json.RawMessage
	Principal Principal
	RequestID string
	// Signer and SignerRole identify who signed the request, for audit
	// logs, whatever the outcome: Signer is the verified kid (a public
	// thumbprint) and SignerRole the role of its grant record, if any
	// (revoked or expired records included). Principal is set only when
	// authority was actually conferred; never use Signer* for decisions.
	Signer     string
	SignerRole Role

	Final string
	// Code is the ok:false code carried by Final, for logs and metrics.
	Code string
	// Notifications are sealed messages the caller must emit (approval
	// requests to owners after a requestAccess): ONE channel's copies.
	Notifications []Sealed
	// Notices are the same notifications unsealed, so a caller emitting on
	// more than one channel seals a separate copy for each (§9, review L1).
	Notices []Notice
	// OwnerEnrolled is set when the first-owner invitation was consumed:
	// the caller takes the claim/invitation QR off the screen (§5.1).
	OwnerEnrolled bool
	// RevokedCtls lists the controllers this admission made inactive
	// (revokeController, including a self-revoke), once the store write
	// committed. A transport that binds connections to controllers (the v2
	// relay, §9) unbinds them.
	RevokedCtls []string

	auth *Authority
	rk   []byte
	ctl  string
}

// Execute reports whether the caller must run the command.
func (adm *Admission) Execute() bool { return adm.Final == "" }

// Reply builds the encrypted, signed reply (§4.1). Use ok=true with code ""
// on success, or ok=false with CodeCommandFailed and the handler's error
// body as res.
func (adm *Admission) Reply(ok bool, code string, res any) (string, error) {
	return adm.auth.sealReply(adm, ok, code, res)
}

// Config configures an Authority.
type Config struct {
	// Dir is the state directory; DefaultDir when empty.
	Dir string
	// DeviceID is the hostname (FF1-XXXXXXXX). Required.
	DeviceID string
	// Enforced is control.enforced from controld.json (§7.2).
	Enforced bool
	// FS defaults to an OSFS.
	FS FS
	// Now defaults to time.Now.
	Now func() time.Time
	// Rand supplies IVs, nk, invitations and gen; defaults to crypto/rand.
	// Tests and the fixture generator inject a deterministic stream.
	Rand io.Reader
}

// Authority is the transport-independent admission authority: it owns the
// identity, the grant store, the replay windows, the pending-request table
// and the invitation, and is the only code that decides who a request is.
// It imports no transport; M2 plugs Open/Reply into the hub and mediator.
type Authority struct {
	mu       sync.Mutex
	store    *Store
	deviceID string
	enforced bool
	now      func() time.Time

	randMu sync.Mutex
	rand   io.Reader

	inv    *invitation
	invGen uint64

	// openSem bounds unauthenticated opens (review M1); openHook is a test
	// seam run while a slot is held.
	openSem        chan struct{}
	openHook       func()
	pending        map[string]*pendingRequest
	denied         map[string]int64 // ctl -> until, owner-denied requests
	lastNewPending int64
	seq            map[string]uint64
	// ep is this daemon run's notification epoch (§9): seq restarts at 1
	// each run, so controllers order by (ep, seq) and accept any new ep.
	ep string
}

// invitation has no attempt counter on purpose: 128 random bits need no
// guess limit, and voiding it after N misses let any LAN peer block
// first-owner and migration enrollment indefinitely (review F2).
type invitation struct {
	value     string
	expiresAt int64
}

// pendingRequest is in memory only: a restart drops pending requests and the
// requester asks again. Its window covers requestAccess/enroll from a ctl
// that has no grant record yet (§6).
type pendingRequest struct {
	ctl         string
	enroll      Enroll
	createdAt   int64
	refreshedAt int64
	expiresAt   int64
	window      Window
	origin      Origin
}

// Origin is where a request came from. It is NOT authority for commands: it
// feeds the per-route pending caps (§5.2), keeps requestAccess LAN-only
// (§5.2) and keeps first-owner enrollment off the device-ID relay (§5.1).
type Origin struct {
	Route Route
	// Remote is the LAN peer's IP (no port); empty when unknown.
	Remote string
	// Known is false for callers that did not say (Open), which count only
	// against the global cap.
	Known bool
}

// New loads (or on first start creates) the control directory. A locked
// directory is not an error: the Authority comes up in StateLocked and
// refuses envelopes, and LockErr says why.
func New(cfg Config) (*Authority, error) {
	if cfg.DeviceID == "" {
		return nil, errors.New("control: Config.DeviceID is required")
	}
	if cfg.Dir == "" {
		cfg.Dir = DefaultDir
	}
	if cfg.FS == nil {
		cfg.FS = &OSFS{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	a := &Authority{
		deviceID: cfg.DeviceID,
		enforced: cfg.Enforced,
		now:      cfg.Now,
		rand:     cfg.Rand,
		pending:  map[string]*pendingRequest{},
		denied:   map[string]int64{},
		openSem:  make(chan struct{}, maxConcurrentOpens),
		seq:      map[string]uint64{},
	}
	a.store = OpenStore(cfg.FS, cfg.Dir, cfg.DeviceID, a.randReader(), cfg.Now())
	ep, err := a.randBytes(epLen)
	if err != nil {
		return nil, err
	}
	a.ep = b64u(ep)
	return a, nil
}

// lockedReader serializes reads from an injected, possibly non-thread-safe
// stream.
type lockedReader struct{ a *Authority }

func (l lockedReader) Read(p []byte) (int, error) {
	l.a.randMu.Lock()
	defer l.a.randMu.Unlock()
	return l.a.rand.Read(p)
}

func (a *Authority) randReader() io.Reader { return lockedReader{a} }

func (a *Authority) randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(a.randReader(), b)
	return b, err
}

// LockErr explains StateLocked; nil otherwise.
func (a *Authority) LockErr() error { return a.store.LockErr() }

// Store exposes the grant store (read-only use: status, tests).
func (a *Authority) Store() *Store { return a.store }

// State returns the current §7.2 state.
func (a *Authority) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked(a.now().Unix())
}

func (a *Authority) stateLocked(now int64) State {
	if a.store.LockErr() != nil {
		return StateLocked
	}
	for _, g := range a.store.Grants() {
		if g.Role == RoleOwner && g.Active(now) {
			if a.enforced {
				return StateEnforced
			}
			return StateEnrolled
		}
	}
	return StateLegacy
}

// Status is the §11 "control" block of /api/v2/status and getDeviceStatus.
type Status struct {
	V       int      `json:"v"`
	State   State    `json:"state"`
	Profile *Profile `json:"profile,omitempty"`
}

// Status returns the public status block. A locked device has no profile it
// can vouch for, so it omits it.
func (a *Authority) Status() Status {
	st := Status{V: 1, State: a.State()}
	if id := a.store.Identity(); id != nil && st.State != StateLocked {
		p := id.Profile()
		st.Profile = &p
	}
	return st
}

// Invitation is the current first-owner invitation (§5.1) plus what the
// claim QR carries next to it (§11).
type Invitation struct {
	Value     string
	ExpiresAt int64
	Pins      Pins
	Gen       string
}

// IssueInvitation creates a fresh invitation, replacing any current one. It
// is refused unless the device is in StateLegacy: once an owner exists,
// later controllers go through requestAccess.
func (a *Authority) IssueInvitation() (Invitation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().Unix()
	if st := a.stateLocked(now); st != StateLegacy {
		return Invitation{}, fmt.Errorf("control: invitation refused in state %s", st)
	}
	b, err := a.randBytes(invLen)
	if err != nil {
		return Invitation{}, err
	}
	a.setInvLocked(&invitation{value: b64u(b), expiresAt: now + invitationTTL})
	return a.invitationLocked(), nil
}

// CurrentInvitation returns the live invitation, if any.
func (a *Authority) CurrentInvitation() (Invitation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inv == nil || a.now().Unix() >= a.inv.expiresAt {
		return Invitation{}, false
	}
	return a.invitationLocked(), true
}

func (a *Authority) invitationLocked() Invitation {
	id := a.store.Identity()
	return Invitation{Value: a.inv.value, ExpiresAt: a.inv.expiresAt, Pins: id.Pins(), Gen: id.gen}
}

// EnsureInvitation returns the live invitation, issuing a fresh one when there
// is none or the current one has less than minRemaining left. The claim-QR
// painter calls it on every paint so the QR never carries a nearly spent
// invitation. Refused unless StateLegacy, like IssueInvitation.
func (a *Authority) EnsureInvitation(minRemaining time.Duration) (Invitation, error) {
	a.mu.Lock()
	live := a.inv != nil && a.now().Add(minRemaining).Unix() < a.inv.expiresAt
	var cur Invitation
	if live {
		cur = a.invitationLocked()
	}
	a.mu.Unlock()
	if live && a.State() == StateLegacy {
		return cur, nil
	}
	return a.IssueInvitation()
}

// Retire deletes the control directory and voids every grant, invitation and
// pending request (factory reset). See Store.Retire.
func (a *Authority) Retire() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.store.Retire()
	// Memory follows the tombstone: if it never committed, nothing changes
	// (§8); once it did, invitation and pending requests are void.
	if errors.Is(a.store.LockErr(), ErrRetired) {
		a.setInvLocked(nil)
		a.pending = map[string]*pendingRequest{}
	}
	return err
}

// ClearInvitation voids the current invitation (QR dismissed, claim taken).
func (a *Authority) ClearInvitation() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setInvLocked(nil)
}

// setInvLocked replaces the invitation and bumps invGen when the value on
// offer actually changes (issued, cleared, consumed).
func (a *Authority) setInvLocked(inv *invitation) {
	if a.inv == nil && inv == nil {
		return
	}
	a.inv = inv
	a.invGen++
}

// InvitationGeneration changes every time an invitation is issued, cleared
// or consumed. A caller that must not race a newly painted invitation (the
// screenshot route, whose capture takes seconds) reads it before and after
// and refuses when it moved (review N1).
func (a *Authority) InvitationGeneration() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.invGen
}

func (a *Authority) approvalCode(ctl string) string {
	return ApprovalCode(ctl, a.store.Identity().Pins().Sig)
}

// ApprovalCode is §5.4: the 6-digit code the owner and requester compare.
// devSigPin is thumb(devSig): the device passes its own, the requester the
// pin it holds. Binding the pin means a LAN man-in-the-middle that served
// the new phone a fake profile (and relays its still-valid JWS to the real
// device) shows a different code on the owner's screen than on the phone.
func ApprovalCode(ctl, devSigPin string) string {
	sum := sha256.Sum256([]byte(approvalCodePrefix + ctl + "|" + devSigPin))
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1000000)
}

// Open runs §3.4 on a compact request. A non-nil error is always a
// *PublicError. Otherwise see Admission. Transports use OpenFrom.
//
// Open is a LAN open from an unknown remote (tests and conformance vectors).
// Every production caller goes through OpenFrom with its real route.
func (a *Authority) Open(compact string) (*Admission, error) {
	return a.OpenFrom(compact, Origin{Route: RouteLAN})
}

// OpenFrom is Open with the request's origin: requestAccess is LAN-only and
// capped per route (§5.2), and enroll with inv is refused on RouteRelayV2
// (§5.1).
func (a *Authority) OpenFrom(compact string, origin Origin) (*Admission, error) {
	if err := a.store.LockErr(); err != nil {
		return nil, publicErr(CodeLocked, err)
	}
	// Steps 1-3 and the claims parse run OUTSIDE a.mu, bounded by openSem:
	// they are unauthenticated work (ECDH, up to 4 MiB of AES-GCM, ES256,
	// a full JSON walk) and must not stall every other admission and State()
	// behind one hostile envelope (review M1). a.mu covers only the window,
	// pending and store mutation below.
	if !a.acquireOpen() {
		return nil, publicErr(CodeBusy, errors.New("too many envelopes being opened"))
	}
	pre, adm, perr := a.authenticate(compact)
	a.releaseOpen()
	if perr != nil {
		return nil, perr
	}
	if adm != nil {
		return adm, nil // an encrypted claim failure; nothing to mutate
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.LockErr(); err != nil {
		return nil, publicErr(CodeLocked, err) // retired while we verified
	}
	now := a.now().Unix()
	// Re-read the record under the lock: the verified key cannot change
	// (ctl = thumb(sig)), but the window, role and revocation can have.
	grant, known := a.store.Grant(pre.adm.ctl)
	if known {
		pre.adm.SignerRole = grant.Role
	}
	c := pre.claims
	a.prunePending(now)
	if pre.futureIat {
		return a.refuseFutureIat(pre.adm, c, grant, known)
	}
	switch c.Op {
	case OpCmd:
		if !known {
			return nil, publicErr(CodeUnknownController, errors.New("kid not in grant store"))
		}
		return a.admitCmd(pre.adm, c, grant, now)
	case OpEnroll:
		if c.Enroll.Inv != "" {
			return a.enrollWithInvitation(pre.adm, c, grant, known, now, origin)
		}
		return a.enrollApproved(pre.adm, c, grant, known, now)
	default:
		return a.requestAccess(pre.adm, c, grant, known, now, origin)
	}
}

// maxEnrollPayload bounds the claims of a request whose kid is not in the
// grant store. Only enroll/requestAccess may come from an unknown kid, and
// their claims are a few hundred bytes; anything larger is refused before
// any JSON walk (review M1).
const maxEnrollPayload = 16 << 10

// maxConcurrentOpens bounds unauthenticated opens in flight, so the hub's
// 64 request slots cannot all be spent on envelope crypto; openWait is how
// long an open waits for a slot before answering busy.
const maxConcurrentOpens = 4

var openWait = time.Second

func (a *Authority) acquireOpen() bool {
	select {
	case a.openSem <- struct{}{}:
	default:
		t := time.NewTimer(openWait)
		defer t.Stop()
		select {
		case a.openSem <- struct{}{}:
		case <-t.C:
			return false
		}
	}
	if a.openHook != nil {
		a.openHook()
	}
	return true
}

func (a *Authority) releaseOpen() { <-a.openSem }

type authenticated struct {
	adm    *Admission
	claims RequestClaims
	// futureIat marks a step-4 iat too far ahead. It is the only claim
	// refusal that a later clock can turn valid, so it is answered under
	// a.mu where the window can be spent (review round 3 L-1).
	futureIat bool
}

// authenticate is §3.4 steps 1-4 (everything that needs no mutable state):
// it returns the authenticated request, or an Admission already finalized
// with an encrypted claim error, or a public error.
func (a *Authority) authenticate(compact string) (authenticated, *Admission, *PublicError) {
	id := a.store.Identity()
	// Step 1: outer JWE.
	jwsBytes, err := openRequest(compact, id.enc, id.encPub.Thumbprint())
	if err != nil {
		return authenticated{}, nil, publicErr(CodeBadEnvelope, err)
	}
	// Step 2: inner JWS, key lookup, signature.
	p, err := parseJWS(string(jwsBytes), typReqJWS)
	if err != nil {
		return authenticated{}, nil, publicErr(CodeBadEnvelope, err)
	}
	grant, known := a.store.Grant(p.kid)
	key := grant.Sig
	if !known {
		if len(p.payload) > maxEnrollPayload {
			return authenticated{}, nil, publicErr(CodeUnknownController, errors.New("kid not in grant store"))
		}
		op, sig := peekClaims(p.payload)
		if (op != OpEnroll && op != OpRequestAccess) || sig == nil || sig.Thumbprint() != p.kid {
			return authenticated{}, nil, publicErr(CodeUnknownController, errors.New("kid not in grant store"))
		}
		key = *sig
	}
	pub, err := key.ECDSA()
	if err != nil || !p.verify(pub) {
		return authenticated{}, nil, publicErr(CodeBadSignature, errors.New("ES256 verification failed"))
	}
	// Step 3: the reply handle.
	h, m, err := parseReplyHandle(p.payload)
	if err != nil {
		return authenticated{}, nil, publicErr(CodeBadEnvelope, err)
	}

	// From here on the request is authentic and every failure is an
	// encrypted reply to the signer (ctl = the verified kid).
	adm := &Admission{auth: a, rk: h.rk, ctl: p.kid, RequestID: h.rid, Signer: p.kid}
	if known {
		adm.SignerRole = grant.Role
	}
	finish := func(code string) (authenticated, *Admission, *PublicError) {
		out, err := a.fail(adm, code, nil)
		if err != nil {
			var pe *PublicError
			errors.As(err, &pe)
			return authenticated{}, nil, pe
		}
		return authenticated{}, out, nil
	}
	c, err := parseRequestClaims(m)
	if err != nil {
		var ce *claimError
		if errors.As(err, &ce) {
			return finish(ce.code)
		}
		return finish(CodeInvalidRequest)
	}
	adm.Op, adm.Command = c.Op, c.Cmd
	now := a.now().Unix()
	// Step 4.
	switch {
	case c.Dev != id.deviceID, c.Gen != id.gen, c.Ctl != p.kid:
		return finish(CodeInvalidRequest)
	case now > c.Exp+expiryGrace:
		return finish(CodeExpired)
	}
	if c.Enroll != nil && c.Enroll.Sig.Thumbprint() != c.Ctl {
		return finish(CodeInvalidRequest)
	}
	return authenticated{adm: adm, claims: c, futureIat: c.Iat > now+futureSkew}, nil, nil
}

// fail finalizes adm with an ok:false reply. If even the reply cannot be
// built (entropy failure) the request degrades to a public error.
func (a *Authority) fail(adm *Admission, code string, res any) (*Admission, error) {
	out, err := a.sealReply(adm, false, code, res)
	if err != nil {
		return nil, publicErr(CodeBadEnvelope, err)
	}
	adm.Final, adm.Code = out, code
	return adm, nil
}

func (a *Authority) succeed(adm *Admission, res any) (*Admission, error) {
	out, err := a.sealReply(adm, true, "", res)
	if err != nil {
		return nil, publicErr(CodeBadEnvelope, err)
	}
	adm.Final = out
	return adm, nil
}

// persistFailure maps a store error to the reply: the request was not
// executed and its ctr may or may not be consumed, so the controller must
// retry with a NEW ctr.
func (a *Authority) persistFailure(adm *Admission) (*Admission, error) {
	return a.fail(adm, CodeInternalError, nil)
}

// refuse answers an authenticated denial from a ctl with a grant record and
// spends its ctr like an accepted request (§6), so a captured refused
// envelope cannot become live after a later promotion or re-approval
// (review D1). A committed but unconfirmed write (ErrDurability) still moved
// the window, so the denial stands.
func (a *Authority) refuse(adm *Admission, ctl string, window Window, code string) (*Admission, error) {
	if err := a.store.Update(setWindow(ctl, window)); err != nil && !errors.Is(err, ErrDurability) {
		return a.persistFailure(adm)
	}
	return a.fail(adm, code, nil)
}

// refuseFutureIat answers a step-4 future iat with invalid_request and
// spends ctr where a window exists, so the envelope cannot be accepted
// verbatim once the device clock catches up.
func (a *Authority) refuseFutureIat(adm *Admission, c RequestClaims, g Grant, known bool) (*Admission, error) {
	if known {
		if err := g.Window.Check(c.Ctr); err != nil {
			return a.fail(adm, CodeReplay, nil)
		}
		return a.refuse(adm, g.Ctl, g.Window.Accept(c.Ctr), CodeInvalidRequest)
	}
	if p, ok := a.pending[c.Ctl]; ok {
		if err := p.window.Check(c.Ctr); err != nil {
			return a.fail(adm, CodeReplay, nil)
		}
		p.window = p.window.Accept(c.Ctr)
	}
	return a.fail(adm, CodeInvalidRequest, nil)
}

// setWindow is an Update step recording w for ctl.
func setWindow(ctl string, w Window) func([]Grant) ([]Grant, error) {
	return func(gs []Grant) ([]Grant, error) {
		i := indexOf(gs, ctl)
		if i < 0 {
			return nil, errors.New("control: grant vanished")
		}
		gs[i].Window = w
		return gs, nil
	}
}

func (a *Authority) admitCmd(adm *Admission, c RequestClaims, g Grant, now int64) (*Admission, error) {
	// Key selection guaranteed a grant record exists for op=cmd.
	if err := g.Window.Check(c.Ctr); err != nil {
		return a.fail(adm, CodeReplay, nil)
	}
	window := g.Window.Accept(c.Ctr)
	if !g.Active(now) {
		return a.refuse(adm, g.Ctl, window, CodeAccessRevoked)
	}
	if a.stateLocked(now) == StateLegacy {
		return a.refuse(adm, g.Ctl, window, CodeNotApproved)
	}
	if !TierOf(c.Cmd).Allows(g.Role) && !isSelfRevoke(c, g) {
		return a.refuse(adm, g.Ctl, window, CodePermissionDenied)
	}
	adm.Command, adm.Request = c.Cmd, c.Req
	adm.Principal = Principal{ControllerID: g.Ctl, Role: g.Role}

	switch c.Cmd {
	case CmdApproveController, CmdRevokeController, CmdListControllers, CmdRegisterController:
		return a.controlCommand(adm, c, window, now)
	}
	// Step 6: durable before the caller executes anything.
	if err := a.store.Update(setWindow(g.Ctl, window)); err != nil {
		return a.persistFailure(adm)
	}
	return adm, nil
}

// isSelfRevoke reports whether c is revokeController naming the signer's own
// ctl: the one owner-tier form a controller grant may send (§5.5), so a
// controller that removes an FF1 does not leave an orphan grant behind. The
// last_owner guard in revoke still applies to an owner revoking itself.
func isSelfRevoke(c RequestClaims, g Grant) bool {
	if c.Cmd != CmdRevokeController {
		return false
	}
	m, err := strictObject(c.Req, []string{"ctl"}, nil)
	if err != nil {
		return false
	}
	return expectString(m, "ctl", g.Ctl) == nil
}

// controlResult is the outcome of an owner control command executed inside
// the same store update that records its replay window.
type controlResult struct {
	res     any
	failure string // non-empty: ok:false command_failed with {"error":failure}
}

func (a *Authority) controlCommand(adm *Admission, c RequestClaims, window Window, now int64) (*Admission, error) {
	var out controlResult
	// Memory-only side effects (pending table, seq) are applied after the
	// store commit, so a failed write leaves them untouched too.
	var approvedPending, deniedPending, revoked, registered string
	err := a.store.Update(func(gs []Grant) ([]Grant, error) {
		gs, err := setWindow(adm.ctl, window)(gs)
		if err != nil {
			return nil, err
		}
		switch c.Cmd {
		case CmdApproveController:
			gs, out, approvedPending = a.approve(gs, adm.ctl, c.Req, now)
		case CmdRevokeController:
			gs, out, deniedPending, revoked = a.revoke(gs, c.Req, now)
		case CmdRegisterController:
			gs, out, registered = a.register(gs, adm.ctl, c.Req, now)
		default:
			out = a.list(gs, adm.ctl, now)
		}
		return gs, nil
	})
	// A committed write is what a restart reads, even when its directory
	// fsync was not confirmed (ErrDurability): the memory-only side effects
	// must follow it, or a retry would act on a state the store already left
	// behind (e.g. re-approve from a pending entry and rotate nk again).
	// The reply is still internal_error; the client re-lists (§4.3).
	committed := err == nil || errors.Is(err, ErrDurability)
	if !committed {
		return a.persistFailure(adm)
	}
	delete(a.pending, approvedPending)
	if registered != "" {
		// The owner added these keys directly: a pending request or a
		// standing deny for the same ctl is moot (§5.6).
		delete(a.pending, registered)
		delete(a.denied, registered)
	}
	if p, ok := a.pending[deniedPending]; ok {
		// An owner's deny holds until the request would have expired, so
		// the requester cannot immediately re-ask and re-notify (§5.2).
		a.denied[deniedPending] = p.expiresAt
		delete(a.pending, deniedPending)
	}
	// The revoked controller's seq is kept: a re-approval continues the
	// sequence instead of restarting below what it already saw (§9).
	if revoked != "" {
		adm.RevokedCtls = append(adm.RevokedCtls, revoked)
	}
	if err != nil {
		return a.persistFailure(adm)
	}
	if out.failure != "" {
		return a.fail(adm, CodeCommandFailed, map[string]string{"error": out.failure})
	}
	return a.succeed(adm, out.res)
}

func (a *Authority) approve(gs []Grant, approver string, req json.RawMessage, now int64) ([]Grant, controlResult, string) {
	m, err := strictObject(req, []string{"ctl", "role"}, []string{"expiresAt"})
	if err != nil {
		return gs, controlResult{failure: "invalid_request"}, ""
	}
	ctl, err1 := stringMember(m, "ctl")
	role, err2 := stringMember(m, "role")
	if err1 != nil || err2 != nil || !Role(role).valid() {
		return gs, controlResult{failure: "invalid_request"}, ""
	}
	expiresAt, ok := grantExpiry(m, Role(role), now)
	if !ok {
		return gs, controlResult{failure: "invalid_request"}, ""
	}
	existing := indexOf(gs, ctl)
	if demotesLastOwner(gs, ctl, Role(role), now) {
		return gs, controlResult{failure: "last_owner"}, ""
	}
	p, ok := a.pending[ctl]
	if !ok {
		// No pending request: a role/expiry change of an ACTIVE grant in
		// place. Keys, nk, window and createdAt are kept.
		if existing < 0 || !gs[existing].Active(now) {
			return gs, controlResult{failure: "not_pending"}, ""
		}
		gs[existing].Role, gs[existing].ExpiresAt, gs[existing].ApprovedBy = Role(role), expiresAt, approver
		return gs, controlResult{res: struct{}{}}, ""
	}
	nk, err := a.randBytes(32)
	if err != nil {
		return gs, controlResult{failure: "internal_error"}, ""
	}
	if activeOthers(gs, ctl, now) >= maxActiveGrants {
		return gs, controlResult{failure: "too_many_controllers"}, ""
	}
	g := Grant{Ctl: ctl, Sig: p.enroll.Sig, Enc: p.enroll.Enc, Role: Role(role), Label: p.enroll.Label,
		Platform: p.enroll.Platform, CreatedAt: now, ApprovedBy: approver, ExpiresAt: expiresAt, NK: [32]byte(nk),
		Window: p.window}
	gs, ok = putGrant(gs, g, now)
	if !ok {
		return gs, controlResult{failure: "too_many_controllers"}, ""
	}
	return gs, controlResult{res: struct{}{}}, ctl
}

// grantExpiry reads the optional expiresAt of approveController and
// registerController. An owner grant cannot expire: the last owner silently
// lapsing would turn an owned device back into a legacy one.
func grantExpiry(m map[string]json.RawMessage, role Role, now int64) (int64, bool) {
	if _, ok := m["expiresAt"]; !ok {
		return 0, true
	}
	v, err := uintMember(m, "expiresAt")
	if err != nil || int64(v) <= now || role == RoleOwner { //nolint:gosec // G115: v <= 2^53-1.
		return 0, false
	}
	return int64(v), true //nolint:gosec // G115: v <= 2^53-1.
}

// demotesLastOwner reports whether giving ctl role would leave no other
// active owner (§5.5).
func demotesLastOwner(gs []Grant, ctl string, role Role, now int64) bool {
	i := indexOf(gs, ctl)
	if i < 0 || !gs[i].Active(now) || gs[i].Role != RoleOwner || role == RoleOwner {
		return false
	}
	for _, g := range gs {
		if g.Role == RoleOwner && g.Active(now) && g.Ctl != ctl {
			return false
		}
	}
	return true
}

// activeOthers counts the active grants other than ctl's.
func activeOthers(gs []Grant, ctl string, now int64) int {
	n := 0
	for _, g := range gs {
		if g.Active(now) && g.Ctl != ctl {
			n++
		}
	}
	return n
}

// putGrant stores g, which gains authority now. An existing record (revoked
// or expired, or re-approved while active) keeps its createdAt and replay
// window, never reset (§6). A new record may evict the longest-inactive one
// at the record cap; false means there is no room.
func putGrant(gs []Grant, g Grant, now int64) ([]Grant, bool) {
	if i := indexOf(gs, g.Ctl); i >= 0 {
		g.CreatedAt, g.Window = gs[i].CreatedAt, gs[i].Window
		gs[i] = g
		return gs, true
	}
	if len(gs) >= maxGrantRecords {
		j := evictable(gs, now)
		if j < 0 {
			return gs, false
		}
		gs = append(gs[:j], gs[j+1:]...)
	}
	return append(gs, g), true
}

// registerResult is registerController's res: the ctl the device derived
// from the scanned sig key, so the owner app can check it registered the
// keys it scanned.
type registerResult struct {
	Ctl       string `json:"ctl"`
	Role      Role   `json:"role"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
}

// register is registerController (§5.6): an owner adds a controller from the
// keys it scanned off the controller's own screen. It is idempotent per ctl:
//   - an active grant keeps its keys, nk, window and createdAt and takes the
//     new role, expiry and label;
//   - a revoked or expired record is reactivated with a fresh nk, keeping
//     its window and createdAt;
//   - a record whose enc differs is refused: ctl = thumb(sig) pins sig, and
//     a second enc for the same sig is never silently swapped in.
//
// It returns the registered ctl for the caller's post-commit cleanup.
func (a *Authority) register(gs []Grant, approver string, req json.RawMessage, now int64) ([]Grant, controlResult, string) {
	bad := controlResult{failure: "invalid_request"}
	m, err := strictObject(req, []string{"sig", "enc", "label", "platform", "role"}, []string{"expiresAt"})
	if err != nil {
		return gs, bad, ""
	}
	sig, err := ParsePublicJWK(m["sig"])
	if err != nil {
		return gs, bad, ""
	}
	enc, err := ParsePublicJWK(m["enc"])
	if err != nil {
		return gs, bad, ""
	}
	label, err1 := stringMember(m, "label")
	platform, err2 := stringMember(m, "platform")
	role, err3 := stringMember(m, "role")
	if err1 != nil || err2 != nil || err3 != nil || !validLabel(label) || !slices.Contains(validPlatforms, platform) ||
		!Role(role).valid() {
		return gs, bad, ""
	}
	expiresAt, ok := grantExpiry(m, Role(role), now)
	if !ok {
		return gs, bad, ""
	}
	ctl := sig.Thumbprint()
	res := controlResult{res: registerResult{Ctl: ctl, Role: Role(role), ExpiresAt: expiresAt}}
	i := indexOf(gs, ctl)
	if i >= 0 && !gs[i].Enc.Equal(enc) {
		return gs, bad, ""
	}
	if demotesLastOwner(gs, ctl, Role(role), now) {
		return gs, controlResult{failure: "last_owner"}, ""
	}
	if i >= 0 && gs[i].Active(now) {
		gs[i].Role, gs[i].ExpiresAt, gs[i].ApprovedBy = Role(role), expiresAt, approver
		gs[i].Label, gs[i].Platform = label, platform
		return gs, res, ctl
	}
	if activeOthers(gs, ctl, now) >= maxActiveGrants {
		return gs, controlResult{failure: "too_many_controllers"}, ""
	}
	nk, err := a.randBytes(32)
	if err != nil {
		return gs, controlResult{failure: "internal_error"}, ""
	}
	g := Grant{Ctl: ctl, Sig: sig, Enc: enc, Role: Role(role), Label: label, Platform: platform, CreatedAt: now,
		ApprovedBy: approver, ExpiresAt: expiresAt, NK: [32]byte(nk)}
	if p, ok := a.pending[ctl]; ok && i < 0 {
		// The same keys asked over the LAN first: the ctrs that request
		// spent stay spent (§6).
		g.Window = p.window
	}
	gs, ok = putGrant(gs, g, now)
	if !ok {
		return gs, controlResult{failure: "too_many_controllers"}, ""
	}
	return gs, res, ctl
}

// recordRetention is how long an inactive record must keep its replay
// window: the latest any envelope signed while it was active can still pass
// §3.4 step 4 (iat up to futureSkew ahead, exp-iat up to maxLifetime, plus
// expiryGrace).
const recordRetention = futureSkew + maxLifetime + expiryGrace

// evictable returns the index of the record that has been inactive longest,
// once past recordRetention, or -1. Active records are never evicted. It
// frees a slot at the record cap, which revoked and expired records would
// otherwise hold forever.
func evictable(gs []Grant, now int64) int {
	best, bestSince := -1, int64(0)
	for i, g := range gs {
		if g.Active(now) {
			continue
		}
		since := g.RevokedAt
		if since == 0 || (g.ExpiresAt != 0 && g.ExpiresAt < since) {
			since = g.ExpiresAt
		}
		if now-since <= recordRetention {
			continue
		}
		if best < 0 || since < bestSince {
			best, bestSince = i, since
		}
	}
	return best
}

// revoke returns the next records, the result, a pending request it denied
// and a grant it revoked (for the caller's post-commit cleanup).
func (a *Authority) revoke(gs []Grant, req json.RawMessage, now int64) ([]Grant, controlResult, string, string) {
	bad := controlResult{failure: "invalid_request"}
	m, err := strictObject(req, []string{"ctl"}, nil)
	if err != nil {
		return gs, bad, "", ""
	}
	ctl, err := stringMember(m, "ctl")
	if err != nil {
		return gs, bad, "", ""
	}
	i := indexOf(gs, ctl)
	if i >= 0 && gs[i].RevokedAt == 0 && !gs[i].Active(now) {
		// An expired grant is revoked durably, so a wall-clock step back
		// cannot revive it.
		gs[i].RevokedAt = now
		denied := ""
		if _, ok := a.pending[ctl]; ok {
			denied = ctl
		}
		return gs, controlResult{res: struct{}{}}, denied, ctl
	}
	if i < 0 || !gs[i].Active(now) {
		// Revoking a pending request denies it.
		if _, ok := a.pending[ctl]; ok {
			return gs, controlResult{res: struct{}{}}, ctl, ""
		}
		return gs, controlResult{failure: "not_found"}, "", ""
	}
	if gs[i].Role == RoleOwner {
		owners := 0
		for _, g := range gs {
			if g.Role == RoleOwner && g.Active(now) {
				owners++
			}
		}
		if owners <= 1 {
			return gs, controlResult{failure: "last_owner"}, "", ""
		}
	}
	gs[i].RevokedAt = now
	return gs, controlResult{res: struct{}{}}, "", ctl
}

type controllerView struct {
	Ctl       string `json:"ctl"`
	Role      Role   `json:"role"`
	Label     string `json:"label"`
	Platform  string `json:"platform"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	RevokedAt int64  `json:"revokedAt,omitempty"`
	Self      bool   `json:"self"`
}

type pendingView struct {
	Ctl       string `json:"ctl"`
	Label     string `json:"label"`
	Platform  string `json:"platform"`
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
}

func (a *Authority) list(gs []Grant, self string, _ int64) controlResult {
	res := struct {
		Controllers []controllerView `json:"controllers"`
		Pending     []pendingView    `json:"pending"`
	}{Controllers: []controllerView{}, Pending: []pendingView{}}
	for _, g := range gs {
		res.Controllers = append(res.Controllers, controllerView{g.Ctl, g.Role, g.Label, g.Platform, g.CreatedAt, g.ExpiresAt, g.RevokedAt, g.Ctl == self})
	}
	for _, p := range a.sortedPending() {
		res.Pending = append(res.Pending, pendingView{p.ctl, p.enroll.Label, p.enroll.Platform, a.approvalCode(p.ctl), p.expiresAt})
	}
	return controlResult{res: res}
}

func (a *Authority) sortedPending() []*pendingRequest {
	ps := make([]*pendingRequest, 0, len(a.pending))
	for _, p := range a.pending {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(x, y *pendingRequest) int {
		if x.createdAt != y.createdAt {
			return int(x.createdAt - y.createdAt)
		}
		if x.ctl < y.ctl {
			return -1
		}
		return 1
	})
	return ps
}

func (a *Authority) prunePending(now int64) {
	for k, p := range a.pending {
		if now >= p.expiresAt {
			delete(a.pending, k)
		}
	}
	for k, until := range a.denied {
		if now >= until {
			delete(a.denied, k)
		}
	}
}

// LANSourceKey is the per-LAN-source key for the pending cap (§5.2): the
// IPv4 address (IPv4-mapped IPv6 included), the /64 of a global or ULA IPv6
// address (one host can hold a whole /64), and ONE key for every link-local
// IPv6 address (fe80::/10), whose interface identifiers a host mints at will
// (review L6). Zones ("%eth0") are stripped. An unparsable value is used as is.
func LANSourceKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.WithZone("").Unmap()
	switch {
	case addr.Is4():
		return addr.String()
	case addr.IsLinkLocalUnicast():
		return "fe80::/10"
	}
	pfx, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return pfx.String()
}

// ParseCarrier parses the ff1Envelope `request` object (§10) strictly:
// exactly {"v":1,"jwe":"<compact>"}, no duplicate, case-variant or extra
// member (review I2), and returns the compact.
func ParseCarrier(raw []byte) (string, error) {
	m, err := strictObject(raw, []string{"v", "jwe"}, nil)
	if err != nil {
		return "", err
	}
	if v, err := uintMember(m, "v"); err != nil || v != 1 {
		return "", errors.New("carrier v must be 1")
	}
	jwe, err := stringMember(m, "jwe")
	if err != nil || jwe == "" {
		return "", errors.New("carrier jwe must be a non-empty string")
	}
	return jwe, nil
}

// pendingCapped reports whether a NEW pending request from o would exceed a
// per-route cap (§5.2). The global cap and interval are checked separately.
func (a *Authority) pendingCapped(o Origin, ctl string) bool {
	// Only LAN requests reach the table (requestAccess is LAN-only, §5.2):
	// one LAN host holds at most maxPendingPerLANRemote of it.
	if !o.Known || o.Remote == "" {
		return false
	}
	n := 0
	for _, p := range a.pending {
		if p.ctl != ctl && p.origin.Known && p.origin.Remote == o.Remote {
			n++
		}
	}
	return n >= maxPendingPerLANRemote
}

type enrollResult struct {
	Role      Role    `json:"role"`
	NK        string  `json:"nk"`
	Profile   Profile `json:"profile"`
	ExpiresAt int64   `json:"expiresAt,omitempty"`
}

// enrollWithInvitation is §5.1: the first owner.
func (a *Authority) enrollWithInvitation(adm *Admission, c RequestClaims, g Grant, known bool, now int64, origin Origin) (*Admission, error) {
	window := Window{}
	p, pending := a.pending[c.Ctl]
	switch {
	case known:
		window = g.Window
	case pending:
		window = p.window
	}
	if err := window.Check(c.Ctr); err != nil {
		return a.fail(adm, CodeReplay, nil)
	}
	if known && g.Active(now) && g.ApprovedBy == approvedByInvitation && g.Enc.Equal(c.Enroll.Enc) {
		// A retry after a committed but unconfirmed first-owner write: the
		// invitation is spent, so answer it as the idempotent §5.3 enroll
		// (review round 3 L-2).
		return a.enrollApproved(adm, c, g, known, now)
	}
	invalid := func() (*Admission, error) {
		if known {
			return a.refuse(adm, g.Ctl, window.Accept(c.Ctr), CodeInvitationInvalid)
		}
		if pending {
			p.window = window.Accept(c.Ctr)
		}
		return a.fail(adm, CodeInvitationInvalid, nil)
	}
	if a.stateLocked(now) != StateLegacy || origin.Route == RouteRelayV2 {
		// The device-ID relay is open to anyone who knows the public ID
		// and has no plaintext profile read, so a first owner never
		// enrolls over it; the LAN and the legacy relayer keep the
		// screen-QR migration path (§5.1). Refused before the invitation
		// is compared, so v2 is no oracle for it.
		return invalid()
	}
	if a.inv == nil || now >= a.inv.expiresAt {
		a.setInvLocked(nil)
		return invalid()
	}
	if subtle.ConstantTimeCompare([]byte(c.Enroll.Inv), []byte(a.inv.value)) != 1 {
		return invalid()
	}
	nk, err := a.randBytes(32)
	if err != nil {
		return a.persistFailure(adm)
	}
	owner := Grant{Ctl: c.Ctl, Sig: c.Enroll.Sig, Enc: c.Enroll.Enc, Role: RoleOwner, Label: c.Enroll.Label,
		Platform: c.Enroll.Platform, CreatedAt: now, ApprovedBy: approvedByInvitation, NK: [32]byte(nk),
		Window: window.Accept(c.Ctr)}
	err = a.store.Update(func(gs []Grant) ([]Grant, error) {
		if i := indexOf(gs, c.Ctl); i >= 0 {
			owner.CreatedAt = gs[i].CreatedAt
			gs[i] = owner
			return gs, nil
		}
		return append(gs, owner), nil
	})
	if errors.Is(err, ErrDurability) || err == nil {
		// The grant is what a restart reads: the invitation is spent either
		// way. On ErrDurability the controller retries enroll, with or
		// without inv, and gets the same grant back.
		a.setInvLocked(nil)
		adm.OwnerEnrolled = true
	}
	if err != nil {
		return a.persistFailure(adm)
	}
	adm.Principal = Principal{ControllerID: c.Ctl, Role: RoleOwner}
	return a.succeed(adm, enrollResult{Role: RoleOwner, NK: b64u(nk), Profile: a.store.Identity().Profile()})
}

// enrollApproved is §5.3: enroll without inv.
func (a *Authority) enrollApproved(adm *Admission, c RequestClaims, g Grant, known bool, now int64) (*Admission, error) {
	if known {
		if err := g.Window.Check(c.Ctr); err != nil {
			return a.fail(adm, CodeReplay, nil)
		}
		if g.Active(now) {
			if !g.Enc.Equal(c.Enroll.Enc) {
				return a.refuse(adm, g.Ctl, g.Window.Accept(c.Ctr), CodeInvalidRequest)
			}
			if err := a.store.Update(setWindow(g.Ctl, g.Window.Accept(c.Ctr))); err != nil {
				return a.persistFailure(adm)
			}
			adm.Principal = Principal{ControllerID: g.Ctl, Role: g.Role}
			return a.succeed(adm, enrollResult{Role: g.Role, NK: b64u(g.NK[:]), Profile: a.store.Identity().Profile(), ExpiresAt: g.ExpiresAt})
		}
	}
	if known {
		// A record (revoked or expired) keeps its window in the store: the
		// accepted ctr is persisted like requestAccess does (review L5), so
		// a captured pre-approval enroll cannot be replayed later.
		err := a.store.Update(setWindow(g.Ctl, g.Window.Accept(c.Ctr)))
		if err != nil && !errors.Is(err, ErrDurability) {
			return a.persistFailure(adm)
		}
	}
	p, ok := a.pending[c.Ctl]
	if !ok {
		return a.fail(adm, CodeNotApproved, nil)
	}
	if !known {
		if err := p.window.Check(c.Ctr); err != nil {
			return a.fail(adm, CodeReplay, nil)
		}
		p.window = p.window.Accept(c.Ctr)
	}
	return a.fail(adm, CodePending, a.pendingRes(p))
}

// insertPending records a new or refreshed request (only after any store
// write it depends on committed).
func (a *Authority) insertPending(p *pendingRequest, isNew bool, now int64, c RequestClaims) {
	p.refreshedAt = now
	p.enroll = *c.Enroll
	p.expiresAt = min(now+pendingTTL, p.createdAt+pendingMaxLife)
	if isNew {
		a.pending[c.Ctl] = p
		a.lastNewPending = now
	}
}

func (a *Authority) pendingRes(p *pendingRequest) any {
	return map[string]any{"status": "pending", "code": a.approvalCode(p.ctl), "expiresAt": p.expiresAt}
}

// requestAccess is §5.2.
func (a *Authority) requestAccess(adm *Admission, c RequestClaims, g Grant, known bool, now int64, origin Origin) (*Admission, error) {
	p := a.pending[c.Ctl]
	if known {
		if err := g.Window.Check(c.Ctr); err != nil {
			return a.fail(adm, CodeReplay, nil)
		}
	} else if p != nil {
		if err := p.window.Check(c.Ctr); err != nil {
			return a.fail(adm, CodeReplay, nil)
		}
	}
	// Every refusal spends ctr where a window exists (review D1). A key
	// with neither a record nor a pending entry has nothing to spend: a
	// replay of its refused request is no more than any LAN or relayer peer
	// can send with a fresh key.
	refuse := func(code string) (*Admission, error) {
		if known {
			return a.refuse(adm, g.Ctl, g.Window.Accept(c.Ctr), code)
		}
		if p != nil {
			p.window = p.window.Accept(c.Ctr)
		}
		return a.fail(adm, code, nil)
	}
	if a.stateLocked(now) == StateLegacy {
		// No owner exists to approve; the first controller uses the
		// invitation instead.
		return refuse(CodeNotApproved)
	}
	if until, ok := a.denied[c.Ctl]; ok && now < until {
		return refuse(CodeNotApproved)
	}
	if known && g.Active(now) {
		if err := a.store.Update(setWindow(g.Ctl, g.Window.Accept(c.Ctr))); err != nil {
			return a.persistFailure(adm)
		}
		return a.succeed(adm, map[string]any{"status": "approved", "role": g.Role})
	}
	if origin.Route != RouteLAN {
		// Creating or refreshing a pending request is a LAN-only fallback
		// (§5.2): off the LAN a controller is added by an owner
		// (registerController, §5.6), and a relay requester's only pins
		// would be the ones it asks to be approved under. An active grant
		// still learns it is approved above, on any route.
		return refuse(CodeNotApproved)
	}
	notify := true
	isNew := p == nil
	if isNew {
		if len(a.pending) >= maxPending || (a.lastNewPending != 0 && now-a.lastNewPending < pendingNewInterval) ||
			a.pendingCapped(origin, c.Ctl) {
			return refuse(CodeRateLimited)
		}
		p = &pendingRequest{ctl: c.Ctl, createdAt: now, origin: origin}
	} else {
		// A refresh extends the request; it is rate-limited per ctl and
		// re-notifies owners only when what they are shown changed.
		if now-p.refreshedAt < pendingRefreshInterval {
			return refuse(CodeRateLimited)
		}
		notify = p.enroll.Label != c.Enroll.Label || p.enroll.Platform != c.Enroll.Platform
	}
	if known {
		// A known (revoked/expired) record keeps its window in the store.
		// Persist BEFORE the request enters the table (review L4): an
		// uncommitted write must leave no pending entry and spend no slot.
		err := a.store.Update(setWindow(g.Ctl, g.Window.Accept(c.Ctr)))
		if err != nil && !errors.Is(err, ErrDurability) {
			return a.persistFailure(adm)
		}
		if err != nil {
			// Committed but unconfirmed: the window moved; record the
			// request so a retry with a new ctr is a refresh.
			a.insertPending(p, isNew, now, c)
			return a.persistFailure(adm)
		}
	} else {
		p.window = p.window.Accept(c.Ctr)
	}
	a.insertPending(p, isNew, now, c)
	body := map[string]any{"ctl": p.ctl, "label": p.enroll.Label, "platform": p.enroll.Platform,
		"code": a.approvalCode(p.ctl), "expiresAt": p.expiresAt}
	for _, owner := range a.store.Grants() {
		if notify && owner.Role == RoleOwner && owner.Active(now) {
			s, err := a.sealLocked(owner, NotificationApprovalRequest, body, now)
			if err == nil {
				adm.Notifications = append(adm.Notifications, s)
				adm.Notices = append(adm.Notices, Notice{Ctl: owner.Ctl, Type: NotificationApprovalRequest, Body: body})
			}
		}
	}
	return a.succeed(adm, a.pendingRes(p))
}

func (a *Authority) sealReply(adm *Admission, ok bool, code string, res any) (string, error) {
	if ok != (code == "") {
		return "", errors.New("control: reply code must be set exactly when ok is false")
	}
	id := a.store.Identity()
	rc := ReplyClaims{V: 1, Dev: id.deviceID, Gen: id.gen, Ctl: adm.ctl, Rid: adm.RequestID,
		Iat: a.now().Unix(), OK: ok, Code: code}
	if res != nil {
		b, err := marshalRes(res)
		if err != nil {
			return "", err
		}
		rc.Res = b
	}
	payload, err := marshalJSON(rc)
	if err != nil {
		return "", err
	}
	jws, err := signJWS(id.sig, jwsHeader(id.sigPub.Thumbprint(), typResJWS), payload)
	if err != nil {
		return "", err
	}
	iv, err := a.randBytes(gcmIVLen)
	if err != nil {
		return "", err
	}
	return sealJWE(replyJWEHeader, adm.rk, iv, []byte(jws))
}

func marshalRes(res any) (json.RawMessage, error) {
	if raw, ok := res.(json.RawMessage); ok {
		if err := checkJSON(raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	return marshalJSON(res)
}

// Seal builds a §9 sealed notification for one active grant.
func (a *Authority) Seal(ctl, typ string, body any) (Sealed, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().Unix()
	g, ok := a.store.Grant(ctl)
	if !ok || !g.Active(now) {
		return Sealed{}, fmt.Errorf("control: no active grant %s", ctl)
	}
	return a.sealLocked(g, typ, body, now)
}

// SealAll seals one copy per active grant (§9 "one per active grant").
func (a *Authority) SealAll(typ string, body any) ([]Sealed, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.LockErr(); err != nil {
		return nil, err
	}
	now := a.now().Unix()
	var out []Sealed
	for _, g := range a.store.Grants() {
		if !g.Active(now) {
			continue
		}
		s, err := a.sealLocked(g, typ, body, now)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (a *Authority) sealLocked(g Grant, typ string, body any, now int64) (Sealed, error) {
	b, err := marshalRes(body)
	if err != nil {
		return Sealed{}, err
	}
	id := a.store.Identity()
	a.seq[g.Ctl]++
	pt, err := marshalJSON(NotificationClaims{V: 1, Dev: id.deviceID, Gen: id.gen, Ctl: g.Ctl, Ep: a.ep, Seq: a.seq[g.Ctl],
		Iat: now, Type: typ, Body: b})
	if err != nil {
		return Sealed{}, err
	}
	iv, err := a.randBytes(gcmIVLen)
	if err != nil {
		return Sealed{}, err
	}
	jwe, err := sealJWE(notificationJWEHeader(g.Ctl), g.NK[:], iv, pt)
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{V: 1, Ctl: g.Ctl, JWE: jwe}, nil
}
