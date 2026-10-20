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
	"net"
	"net/http"
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
	// maxPendingPerLANRemote / maxPendingRelayer are the per-route caps
	// (§5.2) under the global maxPending.
	maxPendingPerLANRemote = 1
	maxPendingRelayer      = 2
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

// PublicErrorBody renders a public error body; also used by Policy denials.
func PublicErrorBody(code string) []byte {
	return []byte(`{"error":{"code":` + jsonString(code) + `}}`)
}

// PublicErrorStatus maps a public code to its LAN HTTP status.
func PublicErrorStatus(code string) int {
	switch code {
	case CodeEnvelopeRequired, CodeEnrollmentRequired, CodeScreenProtected:
		return http.StatusForbidden
	case CodeInvalidMessage:
		return http.StatusBadRequest
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
func (s Sealed) Wire() ([]byte, error) {
	return marshalJSON(struct {
		Type               string `json:"type"`
		NotificationType   string `json:"notification_type"`
		PersistRecordCount int    `json:"persist_record_count"`
		Message            Sealed `json:"message"`
	}{"notification", "sealed", 0, s})
}

// Admission is the outcome of opening an authentic request.
//
// If Final is non-empty the authority has already produced the complete
// reply — an encrypted error, or the result of a control-plane operation it
// executes itself (enroll, requestAccess, approve/revoke/listControllers) —
// and the caller sends Final and executes nothing. Otherwise the request is
// an admitted op=cmd: its replay window is already durable, the caller runs
// Command/Request as Principal and answers with Reply.
type Admission struct {
	Op        Op
	Command   string
	Request   json.RawMessage
	Principal Principal
	RequestID string

	Final string
	// Code is the ok:false code carried by Final, for logs and metrics.
	Code string
	// Notifications are sealed messages the caller must emit (approval
	// requests to owners after a requestAccess).
	Notifications []Sealed
	// OwnerEnrolled is set when the first-owner invitation was consumed:
	// the caller takes the claim/invitation QR off the screen (§5.1).
	OwnerEnrolled bool

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

	inv            *invitation
	invGen         uint64
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

// Origin is where a request came from. It is NOT authority: it only feeds
// the per-route pending caps (§5.2), so one LAN host or the relayer cannot
// hold the whole pending table.
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
func (a *Authority) Open(compact string) (*Admission, error) {
	return a.OpenFrom(compact, Origin{})
}

// OpenFrom is Open with the request's origin, used only for the per-route
// pending caps of requestAccess (§5.2).
func (a *Authority) OpenFrom(compact string, origin Origin) (*Admission, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.LockErr(); err != nil {
		return nil, publicErr(CodeLocked, err)
	}
	id := a.store.Identity()
	now := a.now().Unix()

	// Step 1: outer JWE.
	jwsBytes, err := openRequest(compact, id.enc, id.encPub.Thumbprint())
	if err != nil {
		return nil, publicErr(CodeBadEnvelope, err)
	}
	// Step 2: inner JWS, key lookup, signature.
	p, err := parseJWS(string(jwsBytes), typReqJWS)
	if err != nil {
		return nil, publicErr(CodeBadEnvelope, err)
	}
	grant, known := a.store.Grant(p.kid)
	key := grant.Sig
	if !known {
		op, sig := peekClaims(p.payload)
		if (op != OpEnroll && op != OpRequestAccess) || sig == nil || sig.Thumbprint() != p.kid {
			return nil, publicErr(CodeUnknownController, errors.New("kid not in grant store"))
		}
		key = *sig
	}
	pub, err := key.ECDSA()
	if err != nil || !p.verify(pub) {
		return nil, publicErr(CodeBadSignature, errors.New("ES256 verification failed"))
	}
	// Step 3: the reply handle.
	h, m, err := parseReplyHandle(p.payload)
	if err != nil {
		return nil, publicErr(CodeBadEnvelope, err)
	}

	// From here on the request is authentic and every failure is an
	// encrypted reply to the signer (ctl = the verified kid).
	adm := &Admission{auth: a, rk: h.rk, ctl: p.kid, RequestID: h.rid}
	c, err := parseRequestClaims(m)
	if err != nil {
		return a.failClaim(adm, err)
	}
	adm.Op = c.Op
	// Step 4.
	switch {
	case c.Dev != id.deviceID:
		return a.fail(adm, CodeInvalidRequest, nil)
	case c.Gen != id.gen:
		return a.fail(adm, CodeInvalidRequest, nil)
	case c.Ctl != p.kid:
		return a.fail(adm, CodeInvalidRequest, nil)
	case now > c.Exp+expiryGrace:
		return a.fail(adm, CodeExpired, nil)
	case c.Iat > now+futureSkew:
		return a.fail(adm, CodeInvalidRequest, nil)
	}
	if c.Enroll != nil && c.Enroll.Sig.Thumbprint() != c.Ctl {
		return a.fail(adm, CodeInvalidRequest, nil)
	}
	a.prunePending(now)
	switch c.Op {
	case OpCmd:
		return a.admitCmd(adm, c, grant, now)
	case OpEnroll:
		if c.Enroll.Inv != "" {
			return a.enrollWithInvitation(adm, c, grant, known, now)
		}
		return a.enrollApproved(adm, c, grant, known, now)
	default:
		return a.requestAccess(adm, c, grant, known, now, origin)
	}
}

func (a *Authority) failClaim(adm *Admission, err error) (*Admission, error) {
	var ce *claimError
	if errors.As(err, &ce) {
		return a.fail(adm, ce.code, nil)
	}
	return a.fail(adm, CodeInvalidRequest, nil)
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
	if !g.Active(now) {
		return a.fail(adm, CodeAccessRevoked, nil)
	}
	if a.stateLocked(now) == StateLegacy {
		return a.fail(adm, CodeNotApproved, nil)
	}
	if !TierOf(c.Cmd).Allows(g.Role) {
		return a.fail(adm, CodePermissionDenied, nil)
	}
	adm.Command, adm.Request = c.Cmd, c.Req
	adm.Principal = Principal{ControllerID: g.Ctl, Role: g.Role}
	window := g.Window.Accept(c.Ctr)

	switch c.Cmd {
	case CmdApproveController, CmdRevokeController, CmdListControllers:
		return a.controlCommand(adm, c, window, now)
	}
	// Step 6: durable before the caller executes anything.
	if err := a.store.Update(setWindow(g.Ctl, window)); err != nil {
		return a.persistFailure(adm)
	}
	return adm, nil
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
	var approvedPending, deniedPending, revoked string
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
		default:
			out = a.list(gs, adm.ctl, now)
		}
		return gs, nil
	})
	if err != nil {
		return a.persistFailure(adm)
	}
	delete(a.pending, approvedPending)
	if p, ok := a.pending[deniedPending]; ok {
		// An owner's deny holds until the request would have expired, so
		// the requester cannot immediately re-ask and re-notify (§5.2).
		a.denied[deniedPending] = p.expiresAt
		delete(a.pending, deniedPending)
	}
	// The revoked controller's seq is kept: a re-approval continues the
	// sequence instead of restarting below what it already saw (§9).
	_ = revoked
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
	var expiresAt int64
	if _, ok := m["expiresAt"]; ok {
		v, err := uintMember(m, "expiresAt")
		// An owner grant cannot expire: the last owner silently lapsing
		// would turn an owned device back into a legacy one.
		if err != nil || int64(v) <= now || Role(role) == RoleOwner { //nolint:gosec // G115: v <= 2^53-1.
			return gs, controlResult{failure: "invalid_request"}, ""
		}
		expiresAt = int64(v) //nolint:gosec // G115: v <= 2^53-1.
	}
	existing := indexOf(gs, ctl)
	// Demoting an owner must leave another active owner (§5.5).
	if existing >= 0 && gs[existing].Active(now) && gs[existing].Role == RoleOwner && Role(role) != RoleOwner {
		owners := 0
		for _, g := range gs {
			if g.Role == RoleOwner && g.Active(now) && g.Ctl != ctl {
				owners++
			}
		}
		if owners == 0 {
			return gs, controlResult{failure: "last_owner"}, ""
		}
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
	active := 0
	for _, g := range gs {
		if g.Active(now) && g.Ctl != ctl {
			active++
		}
	}
	if active >= maxActiveGrants {
		return gs, controlResult{failure: "too_many_controllers"}, ""
	}
	g := Grant{Ctl: ctl, Sig: p.enroll.Sig, Enc: p.enroll.Enc, Role: Role(role), Label: p.enroll.Label,
		Platform: p.enroll.Platform, CreatedAt: now, ApprovedBy: approver, ExpiresAt: expiresAt, NK: [32]byte(nk),
		Window: p.window}
	if i := indexOf(gs, ctl); i >= 0 {
		// Re-approval of a revoked (or re-approval of an active) record:
		// the window is the record's, never reset (§6).
		g.CreatedAt, g.Window = gs[i].CreatedAt, gs[i].Window
		gs[i] = g
	} else {
		if len(gs) >= maxGrantRecords {
			return gs, controlResult{failure: "too_many_controllers"}, ""
		}
		gs = append(gs, g)
	}
	return gs, controlResult{res: struct{}{}}, ctl
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
// IPv4 address, or the /64 of an IPv6 address, since one LAN host can hold a
// whole /64 of addresses (review N3). An unparsable value is used as is.
func LANSourceKey(ip string) string {
	addr := net.ParseIP(ip)
	if addr == nil {
		return ip
	}
	if v4 := addr.To4(); v4 != nil {
		return v4.String()
	}
	return addr.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// pendingCapped reports whether a NEW pending request from o would exceed a
// per-route cap (§5.2). The global cap and interval are checked separately.
func (a *Authority) pendingCapped(o Origin, ctl string) bool {
	if !o.Known {
		return false
	}
	lan, relayer := 0, 0
	for _, p := range a.pending {
		if p.ctl == ctl || !p.origin.Known {
			continue
		}
		switch {
		case p.origin.Route == RouteLAN && o.Route == RouteLAN && p.origin.Remote == o.Remote && o.Remote != "":
			lan++
		case p.origin.Route == RouteRelayer:
			relayer++
		}
	}
	if o.Route == RouteLAN {
		return lan >= maxPendingPerLANRemote
	}
	return relayer >= maxPendingRelayer
}

type enrollResult struct {
	Role      Role    `json:"role"`
	NK        string  `json:"nk"`
	Profile   Profile `json:"profile"`
	ExpiresAt int64   `json:"expiresAt,omitempty"`
}

// enrollWithInvitation is §5.1: the first owner.
func (a *Authority) enrollWithInvitation(adm *Admission, c RequestClaims, g Grant, known bool, now int64) (*Admission, error) {
	window := Window{}
	if known {
		window = g.Window
	}
	if err := window.Check(c.Ctr); err != nil {
		return a.fail(adm, CodeReplay, nil)
	}
	if a.stateLocked(now) != StateLegacy {
		return a.fail(adm, CodeInvitationInvalid, nil)
	}
	if a.inv == nil || now >= a.inv.expiresAt {
		a.setInvLocked(nil)
		return a.fail(adm, CodeInvitationInvalid, nil)
	}
	if subtle.ConstantTimeCompare([]byte(c.Enroll.Inv), []byte(a.inv.value)) != 1 {
		return a.fail(adm, CodeInvitationInvalid, nil)
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
		// way. On ErrDurability the controller retries enroll without inv
		// and gets the same grant back.
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
				return a.fail(adm, CodeInvalidRequest, nil)
			}
			if err := a.store.Update(setWindow(g.Ctl, g.Window.Accept(c.Ctr))); err != nil {
				return a.persistFailure(adm)
			}
			adm.Principal = Principal{ControllerID: g.Ctl, Role: g.Role}
			return a.succeed(adm, enrollResult{Role: g.Role, NK: b64u(g.NK[:]), Profile: a.store.Identity().Profile(), ExpiresAt: g.ExpiresAt})
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

func (a *Authority) pendingRes(p *pendingRequest) any {
	return map[string]any{"status": "pending", "code": a.approvalCode(p.ctl), "expiresAt": p.expiresAt}
}

// requestAccess is §5.2.
func (a *Authority) requestAccess(adm *Admission, c RequestClaims, g Grant, known bool, now int64, origin Origin) (*Admission, error) {
	if a.stateLocked(now) == StateLegacy {
		// No owner exists to approve; the first controller uses the
		// invitation instead.
		return a.fail(adm, CodeNotApproved, nil)
	}
	if until, ok := a.denied[c.Ctl]; ok && now < until {
		return a.fail(adm, CodeNotApproved, nil)
	}
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
	if known && g.Active(now) {
		if err := a.store.Update(setWindow(g.Ctl, g.Window.Accept(c.Ctr))); err != nil {
			return a.persistFailure(adm)
		}
		return a.succeed(adm, map[string]any{"status": "approved", "role": g.Role})
	}
	notify := true
	if p == nil {
		if len(a.pending) >= maxPending || (a.lastNewPending != 0 && now-a.lastNewPending < pendingNewInterval) ||
			a.pendingCapped(origin, c.Ctl) {
			return a.fail(adm, CodeRateLimited, nil)
		}
		p = &pendingRequest{ctl: c.Ctl, createdAt: now, origin: origin}
		a.pending[c.Ctl] = p
		a.lastNewPending = now
	} else {
		// A refresh extends the request; it is rate-limited per ctl and
		// re-notifies owners only when what they are shown changed.
		if now-p.refreshedAt < pendingRefreshInterval {
			return a.fail(adm, CodeRateLimited, nil)
		}
		notify = p.enroll.Label != c.Enroll.Label || p.enroll.Platform != c.Enroll.Platform
	}
	p.refreshedAt = now
	if known {
		// A known (revoked/expired) record keeps its window in the store.
		if err := a.store.Update(setWindow(g.Ctl, g.Window.Accept(c.Ctr))); err != nil {
			return a.persistFailure(adm)
		}
	} else {
		p.window = p.window.Accept(c.Ctr)
	}
	p.enroll = *c.Enroll
	p.expiresAt = min(now+pendingTTL, p.createdAt+pendingMaxLife)
	body := map[string]any{"ctl": p.ctl, "label": p.enroll.Label, "platform": p.enroll.Platform,
		"code": a.approvalCode(p.ctl), "expiresAt": p.expiresAt}
	for _, owner := range a.store.Grants() {
		if notify && owner.Role == RoleOwner && owner.Active(now) {
			s, err := a.sealLocked(owner, NotificationApprovalRequest, body, now)
			if err == nil {
				adm.Notifications = append(adm.Notifications, s)
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
