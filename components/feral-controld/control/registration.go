package control

// Owner-initiated registration (docs/ff1-control-v1.md §5.6): the controller
// QR and the sealed registration result that the owner's app sends back over
// the pairing channel. The device never runs this code: it is the reference
// the shared fixtures are generated from, and what the app and ff-cli
// implementations are checked against.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	// ControllerQRPrefix starts every controller QR payload.
	ControllerQRPrefix = "ff1-ctl:1|"
	// RegistrationType is the plaintext result's type.
	RegistrationType = "ff1_controller_registration"
	// RegistrationSealedType is the wire message's type.
	RegistrationSealedType = "ff1_controller_registration_sealed"
	registrationLabel      = "ff1-controller-registration/v1"
	psLen                  = 16
	// controllerQRFields is the field count after the prefix; the label is
	// last and may itself contain "|".
	controllerQRFields = 9
)

var channelIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// ControllerQR is a parsed `ff1-ctl:1|…` payload (§5.6.1).
type ControllerQR struct {
	ChannelID string
	MinterKey *ecdh.PublicKey
	Sig       PublicJWK
	Enc       PublicJWK
	PS        []byte
	Platform  string
	Label     string
}

// Ctl is the controller ID the QR's sig key will have once registered.
func (q ControllerQR) Ctl() string { return q.Sig.Thumbprint() }

// String renders the QR payload.
func (q ControllerQR) String() string {
	return ControllerQRPrefix + strings.Join([]string{q.ChannelID, b64u(q.MinterKey.Bytes()),
		b64u(q.Sig.X[:]), b64u(q.Sig.Y[:]), b64u(q.Enc.X[:]), b64u(q.Enc.Y[:]), b64u(q.PS), q.Platform, q.Label}, "|")
}

// ParseControllerQR parses and validates a controller QR payload.
func ParseControllerQR(s string) (ControllerQR, error) {
	var q ControllerQR
	rest, ok := strings.CutPrefix(s, ControllerQRPrefix)
	if !ok {
		return q, errors.New("control: not an ff1-ctl:1 payload")
	}
	f := strings.SplitN(rest, "|", controllerQRFields)
	if len(f) != controllerQRFields {
		return q, errors.New("control: controller QR has too few fields")
	}
	if !channelIDPattern.MatchString(f[0]) {
		return q, errors.New("control: controller QR channelId is invalid")
	}
	q.ChannelID = f[0]
	var err error
	if q.MinterKey, err = parseSEC1Point(f[1]); err != nil {
		return q, fmt.Errorf("control: controller QR minterKey: %w", err)
	}
	if q.Sig, err = jwkFromCoords(f[2], f[3]); err != nil {
		return q, fmt.Errorf("control: controller QR sig: %w", err)
	}
	if q.Enc, err = jwkFromCoords(f[4], f[5]); err != nil {
		return q, fmt.Errorf("control: controller QR enc: %w", err)
	}
	if q.PS, err = b64uDecode(f[6]); err != nil || len(q.PS) != psLen {
		return q, errors.New("control: controller QR ps must be b64u of 16 bytes")
	}
	if !slices.Contains(validPlatforms, f[7]) {
		return q, errors.New("control: controller QR platform is invalid")
	}
	if !validLabel(f[8]) {
		return q, errors.New("control: controller QR label is invalid")
	}
	q.Platform, q.Label = f[7], f[8]
	return q, nil
}

func isV1(m map[string]json.RawMessage) bool {
	v, err := uintMember(m, "v")
	return err == nil && v == 1
}

func parseSEC1Point(s string) (*ecdh.PublicKey, error) {
	b, err := b64uDecode(s)
	if err != nil || len(b) != 1+2*p256CoordLen || b[0] != 4 {
		return nil, errors.New("must be b64u of a 65-byte uncompressed P-256 point")
	}
	return ecdh.P256().NewPublicKey(b)
}

func jwkFromCoords(x, y string) (PublicJWK, error) {
	return ParsePublicJWK([]byte(`{"kty":"EC","crv":"P-256","x":` + jsonString(x) + `,"y":` + jsonString(y) + `}`))
}

// RegistrationResult is the §5.6.2 plaintext. Profile, DeviceName and Role
// are present exactly when Outcome is "approved".
type RegistrationResult struct {
	V          int      `json:"v"`
	Type       string   `json:"type"`
	ChannelID  string   `json:"channelId"`
	PS         string   `json:"ps"`
	Ctl        string   `json:"ctl"`
	Outcome    string   `json:"outcome"`
	Profile    *Profile `json:"profile,omitempty"`
	DeviceName string   `json:"deviceName,omitempty"`
	Role       Role     `json:"role,omitempty"`
	ExpiresAt  int64    `json:"expiresAt,omitempty"`
	TopicID    string   `json:"topicId,omitempty"`
}

// sealedRegistration is the wire message on the pairing channel.
type sealedRegistration struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	EPK  string `json:"epk"`
	IV   string `json:"iv"`
	CT   string `json:"ct"`
}

// registrationKey derives the AES-256-GCM key and AAD (§5.6.2).
func registrationKey(z []byte, channelID, minterKey, epk string) ([]byte, []byte, error) {
	salt := sha256.Sum256([]byte(ControllerQRPrefix + channelID))
	key, err := hkdf.Key(sha256.New, z, salt[:], registrationLabel, 32)
	if err != nil {
		return nil, nil, err
	}
	return key, []byte(registrationLabel + "|" + channelID + "|" + minterKey + "|" + epk), nil
}

// SealRegistration is the owner side: encrypt res to the QR's minter key
// under the ephemeral key epk with iv (12 bytes, never reused for an epk).
func SealRegistration(q ControllerQR, res RegistrationResult, epk *ecdh.PrivateKey, iv []byte) ([]byte, error) {
	pt, err := marshalJSON(res)
	if err != nil {
		return nil, err
	}
	return SealRegistrationBytes(q, pt, epk, iv)
}

// SealRegistrationBytes is SealRegistration over an exact plaintext.
func SealRegistrationBytes(q ControllerQR, pt []byte, epk *ecdh.PrivateKey, iv []byte) ([]byte, error) {
	if len(iv) != 12 {
		return nil, errors.New("control: registration iv must be 12 bytes")
	}
	z, err := epk.ECDH(q.MinterKey)
	if err != nil {
		return nil, err
	}
	epkB64 := b64u(epk.PublicKey().Bytes())
	key, aad, err := registrationKey(z, q.ChannelID, b64u(q.MinterKey.Bytes()), epkB64)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return marshalJSON(sealedRegistration{V: 1, Type: RegistrationSealedType, EPK: epkB64, IV: b64u(iv),
		CT: b64u(gcm.Seal(nil, iv, pt, aad))})
}

// OpenRegistration is the controller side: decrypt a sealed result with the
// minter key behind q and check every binding a controller MUST check
// (§5.6.3) except the optional configured-device match, which wantDev
// enables when non-empty. now bounds expiresAt.
func OpenRegistration(q ControllerQR, minter *ecdh.PrivateKey, wire []byte, wantDev string, now int64) (RegistrationResult, error) {
	var res RegistrationResult
	if !minter.PublicKey().Equal(q.MinterKey) {
		return res, errors.New("control: minter key does not match the QR")
	}
	m, err := strictObject(wire, []string{"v", "type", "epk", "iv", "ct"}, nil)
	if err != nil {
		return res, fmt.Errorf("control: sealed registration: %w", err)
	}
	if !isV1(m) || expectString(m, "type", RegistrationSealedType) != nil {
		return res, errors.New("control: sealed registration has the wrong v or type")
	}
	epkB64, err1 := stringMember(m, "epk")
	ivB64, err2 := stringMember(m, "iv")
	ctB64, err3 := stringMember(m, "ct")
	if err1 != nil || err2 != nil || err3 != nil {
		return res, errors.New("control: sealed registration members must be strings")
	}
	epk, err := parseSEC1Point(epkB64)
	if err != nil {
		return res, fmt.Errorf("control: sealed registration epk: %w", err)
	}
	iv, err := b64uDecode(ivB64)
	if err != nil || len(iv) != 12 {
		return res, errors.New("control: sealed registration iv must be 12 bytes")
	}
	ct, err := b64uDecode(ctB64)
	if err != nil {
		return res, errors.New("control: sealed registration ct is not b64u")
	}
	z, err := minter.ECDH(epk)
	if err != nil {
		return res, err
	}
	key, aad, err := registrationKey(z, q.ChannelID, b64u(q.MinterKey.Bytes()), epkB64)
	if err != nil {
		return res, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return res, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return res, err
	}
	pt, err := gcm.Open(nil, iv, ct, aad)
	if err != nil {
		return res, errors.New("control: sealed registration does not decrypt")
	}
	return parseRegistrationResult(q, pt, wantDev, now)
}

func parseRegistrationResult(q ControllerQR, pt []byte, wantDev string, now int64) (RegistrationResult, error) {
	var res RegistrationResult
	base := []string{"v", "type", "channelId", "ps", "ctl", "outcome"}
	m, err := strictObject(pt, base, []string{"profile", "deviceName", "role", "expiresAt", "topicId"})
	if err != nil {
		return res, fmt.Errorf("control: registration result: %w", err)
	}
	if !isV1(m) || expectString(m, "type", RegistrationType) != nil {
		return res, errors.New("control: registration result has the wrong v or type")
	}
	res.V, res.Type = 1, RegistrationType
	if expectString(m, "channelId", q.ChannelID) != nil {
		return res, errors.New("control: registration result is for another channel")
	}
	res.ChannelID = q.ChannelID
	ps, err := stringMember(m, "ps")
	if err != nil || subtle.ConstantTimeCompare([]byte(ps), []byte(b64u(q.PS))) != 1 {
		return res, errors.New("control: registration result ps does not match")
	}
	res.PS = ps
	if expectString(m, "ctl", q.Ctl()) != nil {
		return res, errors.New("control: registration result is for another controller")
	}
	res.Ctl = q.Ctl()
	if res.Outcome, err = stringMember(m, "outcome"); err != nil {
		return res, errors.New("control: registration result outcome must be a string")
	}
	switch res.Outcome {
	case "rejected":
		if len(m) != len(base) {
			return res, errors.New("control: a rejected registration carries no grant members")
		}
		return res, nil
	case "approved":
	default:
		return res, errors.New("control: registration result outcome is invalid")
	}
	for _, k := range []string{"profile", "deviceName", "role"} {
		if _, ok := m[k]; !ok {
			return res, fmt.Errorf("control: an approved registration needs %s", k)
		}
	}
	p, err := parseProfile(m["profile"])
	if err != nil {
		return res, err
	}
	if wantDev != "" && p.Dev != wantDev {
		return res, errors.New("control: registration result is for another device")
	}
	res.Profile = &p
	if res.DeviceName, err = stringMember(m, "deviceName"); err != nil || !validLabel(res.DeviceName) {
		return res, errors.New("control: registration result deviceName is invalid")
	}
	role, err := stringMember(m, "role")
	if err != nil || !Role(role).valid() {
		return res, errors.New("control: registration result role is invalid")
	}
	res.Role = Role(role)
	if res.ExpiresAt, err = optionalExpiry(m, res.Role, now); err != nil {
		return res, err
	}
	if _, ok := m["topicId"]; ok {
		if res.TopicID, err = stringMember(m, "topicId"); err != nil || res.TopicID == "" {
			return res, errors.New("control: registration result topicId must be a non-empty string")
		}
	}
	return res, nil
}

func optionalExpiry(m map[string]json.RawMessage, role Role, now int64) (int64, error) {
	if _, ok := m["expiresAt"]; !ok {
		return 0, nil
	}
	v, err := uintMember(m, "expiresAt")
	if err != nil || int64(v) <= now || role == RoleOwner { //nolint:gosec // G115: v <= 2^53-1.
		return 0, errors.New("control: registration result expiresAt is invalid")
	}
	return int64(v), nil //nolint:gosec // G115: v <= 2^53-1.
}

// parseProfile is the strict §2 profile parse a controller pins from.
func parseProfile(raw json.RawMessage) (Profile, error) {
	var p Profile
	m, err := strictObject(raw, []string{"v", "dev", "gen", "sig", "enc"}, nil)
	if err != nil || !isV1(m) {
		return p, errors.New("control: registration result profile is invalid")
	}
	p.V = 1
	if p.Dev, err = stringMember(m, "dev"); err != nil || p.Dev == "" {
		return p, errors.New("control: registration result profile dev is invalid")
	}
	if p.Gen, err = stringMember(m, "gen"); err != nil {
		return p, errors.New("control: registration result profile gen is invalid")
	}
	if g, err := b64uDecode(p.Gen); err != nil || len(g) != 16 {
		return p, errors.New("control: registration result profile gen must be b64u of 16 bytes")
	}
	if p.Sig, err = ParsePublicJWK(m["sig"]); err != nil {
		return p, errors.New("control: registration result profile sig is invalid")
	}
	if p.Enc, err = ParsePublicJWK(m["enc"]); err != nil {
		return p, errors.New("control: registration result profile enc is invalid")
	}
	return p, nil
}
