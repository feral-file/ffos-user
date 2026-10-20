package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// Op is the request operation (§3.3).
type Op string

const (
	OpCmd           Op = "cmd"
	OpEnroll        Op = "enroll"
	OpRequestAccess Op = "requestAccess"
)

const (
	maxLifetime   = 120 // exp - iat upper bound, seconds (§3.3)
	expiryGrace   = 30  // now <= exp + 30 (§3.4 step 4)
	futureSkew    = 300 // iat <= now + 300
	invLen        = 16
	rkLen         = 32
	epLen         = 8 // notification epoch, §9
	maxCmdNameLen = 64
)

var ridPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Enroll is the §5 enroll object.
type Enroll struct {
	Sig      PublicJWK `json:"sig"`
	Enc      PublicJWK `json:"enc"`
	Label    string    `json:"label"`
	Platform string    `json:"platform"`
	Inv      string    `json:"inv,omitempty"`
}

// RequestClaims are the §3.3 claims. MarshalJSON emits the canonical member
// order; parseRequestClaims is the strict device-side decoder.
type RequestClaims struct {
	V      int             `json:"v"`
	Dev    string          `json:"dev"`
	Gen    string          `json:"gen"`
	Ctl    string          `json:"ctl"`
	Rid    string          `json:"rid"`
	Ctr    uint64          `json:"ctr"`
	Iat    int64           `json:"iat"`
	Exp    int64           `json:"exp"`
	Op     Op              `json:"op"`
	RK     string          `json:"rk"`
	Cmd    string          `json:"cmd,omitempty"`
	Req    json.RawMessage `json:"req,omitempty"`
	Enroll *Enroll         `json:"enroll,omitempty"`
}

// Marshal encodes the claims as the JWS payload.
func (c RequestClaims) Marshal() ([]byte, error) { return marshalJSON(c) }

// claimError is a step-3 failure: the request is authentic, so it gets an
// encrypted reply with this code.
type claimError struct {
	code string
	msg  string
}

func (e *claimError) Error() string { return e.code + ": " + e.msg }

func invalidRequest(format string, args ...any) error {
	return &claimError{code: CodeInvalidRequest, msg: fmt.Sprintf(format, args...)}
}

var requestClaimsRequired = []string{"v", "dev", "gen", "ctl", "rid", "ctr", "iat", "exp", "op", "rk"}
var requestClaimsOptional = []string{"cmd", "req", "enroll"}

// replyHandle is what the device needs to answer: the reply key and the
// rid to echo. Without both no encrypted reply is possible, so their
// failure is a public bad_envelope (§3.4).
type replyHandle struct {
	rk  []byte
	rid string
}

// peekClaims reads only what key selection needs before the signature is
// verified (§3.4 step 2): op and, for enrollment ops, enroll.sig. Nothing it
// returns is trusted until the signature over the payload verifies.
func peekClaims(payload []byte) (Op, *PublicJWK) {
	var p struct {
		Op     Op `json:"op"`
		Enroll *struct {
			Sig json.RawMessage `json:"sig"`
		} `json:"enroll"`
	}
	if checkJSON(payload) != nil || json.Unmarshal(payload, &p) != nil {
		return "", nil
	}
	if p.Enroll == nil {
		return p.Op, nil
	}
	k, err := ParsePublicJWK(p.Enroll.Sig)
	if err != nil {
		return p.Op, nil
	}
	return p.Op, &k
}

// parseReplyHandle extracts rk and rid from a verified payload.
func parseReplyHandle(payload []byte) (replyHandle, map[string]json.RawMessage, error) {
	if err := checkJSON(payload); err != nil || !isJSONObject(payload) {
		return replyHandle{}, nil, errors.New("claims are not a JSON object")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return replyHandle{}, nil, err
	}
	rkStr, err := stringMember(m, "rk")
	if err != nil {
		return replyHandle{}, nil, err
	}
	rk, err := b64uDecode(rkStr)
	if err != nil || len(rk) != rkLen {
		return replyHandle{}, nil, errors.New("rk must be b64u(32 bytes)")
	}
	rid, err := stringMember(m, "rid")
	if err != nil || !ridPattern.MatchString(rid) {
		return replyHandle{}, nil, errors.New("rid must be 1-64 chars [A-Za-z0-9_-]")
	}
	return replyHandle{rk: rk, rid: rid}, m, nil
}

// parseRequestClaims is §3.3 + the static half of §3.4 step 4. Time checks
// are separate (checkTimes) because the order of public codes matters.
func parseRequestClaims(m map[string]json.RawMessage) (RequestClaims, error) {
	for k := range m {
		if !slices.Contains(requestClaimsRequired, k) && !slices.Contains(requestClaimsOptional, k) {
			return RequestClaims{}, invalidRequest("unexpected claim %q", k)
		}
	}
	for _, k := range requestClaimsRequired {
		if _, ok := m[k]; !ok {
			return RequestClaims{}, invalidRequest("missing claim %q", k)
		}
	}
	var c RequestClaims
	v, err := uintMember(m, "v")
	if err != nil || v != 1 {
		return c, invalidRequest("v must be 1")
	}
	c.V = 1
	for _, f := range []struct {
		key string
		dst *string
	}{{"dev", &c.Dev}, {"gen", &c.Gen}, {"ctl", &c.Ctl}, {"rid", &c.Rid}, {"rk", &c.RK}} {
		s, err := stringMember(m, f.key)
		if err != nil {
			return c, invalidRequest("%v", err)
		}
		*f.dst = s
	}
	if c.Ctr, err = uintMember(m, "ctr"); err != nil || c.Ctr == 0 {
		return c, invalidRequest("ctr must be an integer >= 1")
	}
	iat, err := uintMember(m, "iat")
	if err != nil {
		return c, invalidRequest("%v", err)
	}
	exp, err := uintMember(m, "exp")
	if err != nil {
		return c, invalidRequest("%v", err)
	}
	c.Iat, c.Exp = int64(iat), int64(exp) //nolint:gosec // G115: both <= 2^53-1.
	if d := c.Exp - c.Iat; d < 1 || d > maxLifetime {
		return c, invalidRequest("exp - iat = %d, want [1, %d]", d, maxLifetime)
	}
	op, err := stringMember(m, "op")
	if err != nil {
		return c, invalidRequest("%v", err)
	}
	c.Op = Op(op)
	_, hasCmd := m["cmd"]
	_, hasReq := m["req"]
	_, hasEnroll := m["enroll"]
	switch c.Op {
	case OpCmd:
		if !hasCmd || !hasReq || hasEnroll {
			return c, invalidRequest("op=cmd requires cmd and req and forbids enroll")
		}
		if c.Cmd, err = stringMember(m, "cmd"); err != nil || c.Cmd == "" || len(c.Cmd) > maxCmdNameLen {
			return c, invalidRequest("cmd must be a non-empty command name")
		}
		if !isJSONObject(m["req"]) {
			return c, invalidRequest("req must be a JSON object")
		}
		c.Req = m["req"]
	case OpEnroll, OpRequestAccess:
		if hasCmd || hasReq || !hasEnroll {
			return c, invalidRequest("op=%s requires enroll and forbids cmd/req", c.Op)
		}
		e, err := parseEnroll(m["enroll"], c.Op)
		if err != nil {
			return c, err
		}
		c.Enroll = &e
	default:
		return c, invalidRequest("unknown op %q", op)
	}
	return c, nil
}

func parseEnroll(raw json.RawMessage, op Op) (Enroll, error) {
	optional := []string{"inv"}
	if op == OpRequestAccess {
		optional = nil // an invitation has no meaning on requestAccess
	}
	m, err := strictObject(raw, []string{"sig", "enc", "label", "platform"}, optional)
	if err != nil {
		return Enroll{}, invalidRequest("enroll: %v", err)
	}
	var e Enroll
	if e.Sig, err = ParsePublicJWK(m["sig"]); err != nil {
		return e, invalidRequest("enroll.sig: %v", err)
	}
	if e.Enc, err = ParsePublicJWK(m["enc"]); err != nil {
		return e, invalidRequest("enroll.enc: %v", err)
	}
	if e.Label, err = stringMember(m, "label"); err != nil || !validLabel(e.Label) {
		return e, invalidRequest("enroll.label must be 1-64 characters")
	}
	if e.Platform, err = stringMember(m, "platform"); err != nil || !slices.Contains(validPlatforms, e.Platform) {
		return e, invalidRequest("enroll.platform must be one of %v", validPlatforms)
	}
	if _, ok := m["inv"]; ok {
		if e.Inv, err = stringMember(m, "inv"); err != nil {
			return e, invalidRequest("enroll.inv must be a string")
		}
	}
	return e, nil
}

// ReplyClaims are the §4.1 reply claims.
type ReplyClaims struct {
	V    int             `json:"v"`
	Dev  string          `json:"dev"`
	Gen  string          `json:"gen"`
	Ctl  string          `json:"ctl"`
	Rid  string          `json:"rid"`
	Iat  int64           `json:"iat"`
	OK   bool            `json:"ok"`
	Code string          `json:"code,omitempty"`
	Res  json.RawMessage `json:"res,omitempty"`
}

// Matches is the §4.2 binding check a controller MUST make: the reply is for
// this device, this generation, this controller and this request.
func (r ReplyClaims) Matches(dev, gen, ctl, rid string) bool {
	return r.Dev == dev && r.Gen == gen && r.Ctl == ctl && r.Rid == rid
}

func parseReplyClaims(raw []byte) (ReplyClaims, error) {
	m, err := strictObject(raw, []string{"v", "dev", "gen", "ctl", "rid", "iat", "ok"}, []string{"code", "res"})
	if err != nil {
		return ReplyClaims{}, malformed("reply claims: %v", err)
	}
	var r ReplyClaims
	if v, err := uintMember(m, "v"); err != nil || v != 1 {
		return r, malformed("reply v")
	}
	if _, err := uintMember(m, "iat"); err != nil {
		return r, malformed("reply iat")
	}
	if r.OK, err = boolMember(m, "ok"); err != nil {
		return r, malformed("reply ok")
	}
	if _, hasCode := m["code"]; hasCode == r.OK {
		return r, malformed("reply code must be present exactly when ok is false")
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, malformed("reply claims: %v", err)
	}
	return r, nil
}

// NotificationClaims are the §9 sealed-notification claims.
type NotificationClaims struct {
	V    int             `json:"v"`
	Dev  string          `json:"dev"`
	Gen  string          `json:"gen"`
	Ctl  string          `json:"ctl"`
	Ep   string          `json:"ep"`
	Seq  uint64          `json:"seq"`
	Iat  int64           `json:"iat"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

func parseNotificationClaims(raw []byte) (NotificationClaims, error) {
	m, err := strictObject(raw, []string{"v", "dev", "gen", "ctl", "ep", "seq", "iat", "type", "body"}, nil)
	if err != nil {
		return NotificationClaims{}, malformed("notification claims: %v", err)
	}
	var n NotificationClaims
	if v, err := uintMember(m, "v"); err != nil || v != 1 {
		return n, malformed("notification v")
	}
	for _, k := range []string{"seq", "iat"} {
		if _, err := uintMember(m, k); err != nil {
			return n, malformed("notification %s", k)
		}
	}
	var ep [epLen]byte
	if err := fixedB64Member(m, "ep", ep[:]); err != nil {
		return n, malformed("notification ep must be b64u(8 bytes): %v", err)
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return n, malformed("notification claims: %v", err)
	}
	return n, nil
}
