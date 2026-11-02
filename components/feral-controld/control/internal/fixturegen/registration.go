package fixturegen

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// registerReq is a registerController request body for keys.json controller
// name, as the owner's app builds it from a scanned controller QR.
func (g *gen) registerReq(name, role, extra string) json.RawMessage {
	c := g.k.Controllers[name]
	return json.RawMessage(`{"sig":` + c.Sig.PublicJSON() + `,"enc":` + c.Enc.PublicJSON() + `,"label":"` + c.Label +
		`","platform":"` + c.Platform + `","role":"` + role + `"` + extra + `}`)
}

// RegistrationVector is positive/controller-registration*.json and the
// negative/registration-*.json files (§5.6).
type RegistrationVector struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"` // registration
	Description string `json:"description"`
	Now         int64  `json:"now"`
	// The controller side: the QR it showed and the minter private key
	// behind it. Controller is the keys.json controller whose sig/enc the
	// QR carries.
	Controller string     `json:"controller"`
	QR         string     `json:"qr"`
	MinterKey  PrivateJWK `json:"minterKey"`
	ChannelID  string     `json:"channelId"`
	PS         string     `json:"ps"`
	// WantDev is the configured device a CLI checks profile.dev against;
	// empty means no check.
	WantDev string `json:"wantDev,omitempty"`
	// The owner side: its ephemeral key and IV, the exact plaintext and
	// the wire message. A positive vector reproduces Wire byte for byte
	// from these; a negative one must be rejected by the controller.
	OwnerEphemeralKey *PrivateJWK     `json:"ownerEphemeralKey,omitempty"`
	IV                string          `json:"iv,omitempty"`
	Plaintext         string          `json:"plaintext,omitempty"`
	Result            json.RawMessage `json:"result,omitempty"`
	Wire              string          `json:"wire"`
	Expect            Expect          `json:"expect"`
}

// ControllerQRCase is one entry of positive/negative controller-qr.json.
type ControllerQRCase struct {
	Description string `json:"description"`
	QR          string `json:"qr"`
	// Parsed fields of a valid QR.
	ChannelID string `json:"channelId,omitempty"`
	MinterKey string `json:"minterKey,omitempty"`
	Ctl       string `json:"ctl,omitempty"`
	Sig       string `json:"sig,omitempty"`
	Enc       string `json:"enc,omitempty"`
	PS        string `json:"ps,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Label     string `json:"label,omitempty"`
}

// ControllerQRVectors is positive/controller-qr.json and
// negative/controller-qr.json.
type ControllerQRVectors struct {
	Name        string             `json:"name"`
	Kind        string             `json:"kind"` // controllerQR
	Description string             `json:"description"`
	Cases       []ControllerQRCase `json:"cases"`
}

func (g *gen) controllerQR(name, label string) (control.ControllerQR, PrivateJWK) {
	c := g.k.Controllers[name]
	minter := jwkFromScalar(scalar("registration/minter/" + label))
	q := control.ControllerQR{ChannelID: "7b0d6e4a-2f51-4c8e-9a43-" + fmt.Sprintf("%012x", derive("registration/channel/" + label)[:6]),
		MinterKey: minter.ECDH().PublicKey(), Sig: pub(c.Sig), Enc: pub(c.Enc), PS: derive("registration/ps/" + label)[:16],
		Platform: c.Platform, Label: c.Label}
	return q, minter
}

func (g *gen) registrationVectors() error {
	q, minter := g.controllerQR("stranger", "main")
	profile := json.RawMessage(g.k.Device.Profile)
	var prof control.Profile
	if err := json.Unmarshal(profile, &prof); err != nil {
		return err
	}
	approved := control.RegistrationResult{V: 1, Type: control.RegistrationType, ChannelID: q.ChannelID,
		PS: b64u(q.PS), Ctl: q.Ctl(), Outcome: "approved", Profile: &prof, DeviceName: "Living Room FF1",
		Role: control.RoleController, ExpiresAt: Now + 86400, TopicID: "fixture-topic-0001"}
	seal := func(name string, res any) (*PrivateJWK, string, string, string, error) {
		epk := jwkFromScalar(scalar("registration/epk/" + name))
		iv := derive("registration/iv/" + name)[:12]
		var pt []byte
		var err error
		if r, ok := res.(control.RegistrationResult); ok {
			pt, err = json.Marshal(r)
		} else {
			pt = []byte(res.(string))
		}
		if err != nil {
			return nil, "", "", "", err
		}
		wire, err := sealRaw(q, pt, epk, iv)
		return &epk, b64u(iv), string(pt), string(wire), err
	}
	emit := func(dir, name, desc, wantDev string, res any, post func(string) string, minterOverride *PrivateJWK) error {
		epk, iv, pt, wire, err := seal(name, res)
		if err != nil {
			return err
		}
		if post != nil {
			wire = post(wire)
		}
		mk := minter
		if minterOverride != nil {
			mk = *minterOverride
		}
		v := RegistrationVector{Name: name, Kind: "registration", Description: desc, Now: Now, Controller: "stranger",
			QR: q.String(), MinterKey: mk, ChannelID: q.ChannelID, PS: b64u(q.PS), WantDev: wantDev,
			OwnerEphemeralKey: epk, IV: iv, Plaintext: pt, Wire: wire}
		_, openErr := control.OpenRegistration(q, mk.ECDH(), []byte(wire), wantDev, Now)
		if dir == "positive" {
			if openErr != nil {
				return fmt.Errorf("%s: %w", name, openErr)
			}
			v.Result = json.RawMessage(pt)
			v.Expect.ReplyOK = true
		} else {
			if openErr == nil {
				return fmt.Errorf("%s: controller accepted", name)
			}
			v.Expect.Reject = true
		}
		g.files[dir+"/"+name+".json"] = Encode(v)
		return nil
	}
	if err := emit("positive", "controller-registration", "Owner app seals an approved result to the controller QR's minter key (§5.6.2); the controller opens and checks it (§5.6.3), with profile.dev matched against wantDev.", DeviceID, approved, nil, nil); err != nil {
		return err
	}
	rejected := control.RegistrationResult{V: 1, Type: control.RegistrationType, ChannelID: q.ChannelID, PS: b64u(q.PS),
		Ctl: q.Ctl(), Outcome: "rejected"}
	if err := emit("positive", "controller-registration-rejected", "The owner declined: a rejected result carries no grant members.", "", rejected, nil, nil); err != nil {
		return err
	}
	ownerExp := approved
	ownerExp.Role = control.RoleOwner
	expired := approved
	expired.ExpiresAt = Now
	other := approved
	other.Ctl = g.k.Controllers["owner"].Ctl
	otherChan := approved
	otherChan.ChannelID = strings.Replace(q.ChannelID, "7b0d", "7b0e", 1)
	wrongPS := approved
	wrongPS.PS = b64u(derive("registration/ps/other")[:16])
	pt, _ := json.Marshal(approved)
	rpt, _ := json.Marshal(rejected)
	repl := func(old, new string) string {
		if !strings.Contains(string(pt), old) {
			panic("fixture plaintext lacks " + old)
		}
		return strings.Replace(string(pt), old, new, 1)
	}
	wireRepl := func(old, new string) func(string) string {
		return func(w string) string {
			if !strings.Contains(w, old) {
				panic("fixture wire lacks " + old)
			}
			return strings.Replace(w, old, new, 1)
		}
	}
	otherMinter := jwkFromScalar(scalar("registration/minter/other"))
	negatives := []struct {
		name, desc, wantDev string
		res                 any
		post                func(string) string
		minter              *PrivateJWK
	}{
		{"registration-wrong-ps", "ps is not the one in the QR.", "", wrongPS, nil, nil},
		{"registration-wrong-channel", "channelId member names another channel (sealed with the right keys).", "", otherChan, nil, nil},
		{"registration-wrong-ctl", "ctl is not thumb(QR sig).", "", other, nil, nil},
		{"registration-wrong-device", "profile.dev is not the configured device.", "FF1-OTHER0001", approved, nil, nil},
		{"registration-owner-expiry", "An owner role with expiresAt.", "", ownerExp, nil, nil},
		{"registration-expired", "expiresAt is not after now.", "", expired, nil, nil},
		{"registration-extra-member", "Plaintext carries an unknown member.", "", repl(`{"v":1`, `{"x":1,"v":1`), nil, nil},
		{"registration-duplicate-member", "Plaintext repeats v.", "", repl(`{"v":1`, `{"v":1,"v":1`), nil, nil},
		{"registration-plaintext-v2", "Plaintext v is 2.", "", repl(`{"v":1`, `{"v":2`), nil, nil},
		{"registration-plaintext-type", "Plaintext type is not ff1_controller_registration.", "", repl(`"type":"ff1_controller_registration"`, `"type":"ff1_controller_handoff"`), nil, nil},
		{"registration-approved-no-profile", "An approved result without profile.", "", repl(`"profile":`+string(profile)+`,`, ""), nil, nil},
		{"registration-approved-no-role", "An approved result without role.", "", repl(`"role":"controller",`, ""), nil, nil},
		{"registration-bad-role", "role is not owner or controller.", "", repl(`"role":"controller"`, `"role":"display"`), nil, nil},
		{"registration-bad-device-name", "deviceName carries a control character.", "", repl(`"deviceName":"Living Room FF1"`, `"deviceName":"Living\u0007Room"`), nil, nil},
		{"registration-rejected-with-role", "A rejected result carrying only an extra role.", "", strings.Replace(string(rpt), `"outcome":"rejected"`, `"outcome":"rejected","role":"controller"`, 1), nil, nil},
		{"registration-wrong-minter", "Opened with a minter key other than the QR's.", "", approved, nil, &otherMinter},
		{"registration-wire-v2", "Wire message v is 2.", "", approved, wireRepl(`{"v":1`, `{"v":2`), nil},
		{"registration-wire-type", "Wire message type is the plaintext type.", "", approved, wireRepl(`"type":"ff1_controller_registration_sealed"`, `"type":"ff1_controller_registration"`), nil},
		{"registration-short-iv", "iv is 11 bytes.", "", approved, func(w string) string {
			var m map[string]string
			_ = json.Unmarshal([]byte(strings.Replace(w, `"v":1,`, "", 1)), &m)
			return strings.Replace(w, `"iv":"`+m["iv"]+`"`, `"iv":"`+b64u(mustB64(m["iv"])[:11])+`"`, 1)
		}, nil},
		{"registration-compressed-epk", "epk in compressed SEC1 form.", "", approved, func(w string) string {
			var m map[string]string
			_ = json.Unmarshal([]byte(strings.Replace(w, `"v":1,`, "", 1)), &m)
			e := mustB64(m["epk"])
			return strings.Replace(w, m["epk"], b64u(append([]byte{2 + e[64]&1}, e[1:33]...)), 1)
		}, nil},
		{"registration-tampered", "One ciphertext character changed.", "", approved, func(w string) string {
			i := strings.Index(w, `"ct":"`) + 6
			b := []byte(w)
			b[i] = map[bool]byte{true: 'B', false: 'A'}[b[i] == 'A']
			return string(b)
		}, nil},
		{"registration-wire-extra-member", "Wire message with an extra member.", "", approved, wireRepl(`{"v":1`, `{"x":1,"v":1`), nil},
	}
	for _, n := range negatives {
		if err := emit("negative", n.name, n.desc, n.wantDev, n.res, n.post, n.minter); err != nil {
			return err
		}
	}
	return g.controllerQRVectors(q)
}

// sealRaw seals exact plaintext bytes, so negative vectors can carry
// plaintexts the typed encoder cannot produce.
func sealRaw(q control.ControllerQR, pt []byte, epk PrivateJWK, iv []byte) ([]byte, error) {
	return control.SealRegistrationBytes(q, pt, epk.ECDH(), iv)
}

func (g *gen) controllerQRVectors(q control.ControllerQR) error {
	pipe := q
	pipe.Label = "Studio | desk"
	var good []ControllerQRCase
	for _, c := range []struct {
		desc string
		q    control.ControllerQR
	}{{"A ff-cli controller QR.", q}, {"The label is the last field and may contain \"|\".", pipe}} {
		parsed, err := control.ParseControllerQR(c.q.String())
		if err != nil {
			return err
		}
		sig, _ := json.Marshal(parsed.Sig)
		enc, _ := json.Marshal(parsed.Enc)
		good = append(good, ControllerQRCase{Description: c.desc, QR: c.q.String(), ChannelID: parsed.ChannelID,
			MinterKey: b64u(parsed.MinterKey.Bytes()), Ctl: parsed.Ctl(), Sig: string(sig), Enc: string(enc),
			PS: b64u(parsed.PS), Platform: parsed.Platform, Label: parsed.Label})
	}
	g.files["positive/controller-qr.json"] = Encode(ControllerQRVectors{Name: "controller-qr", Kind: "controllerQR",
		Description: "ff1-ctl:1|channelId|minterKey|sig.x|sig.y|enc.x|enc.y|ps|platform|label (§5.6.1). minterKey is b64u of the 65-byte uncompressed SEC1 point; split at most 9 times after the prefix.",
		Cases:       good})
	s := q.String()
	f := strings.SplitN(strings.TrimPrefix(s, control.ControllerQRPrefix), "|", 9)
	with := func(i int, v string) string {
		g := append([]string(nil), f...)
		g[i] = v
		return control.ControllerQRPrefix + strings.Join(g, "|")
	}
	compressed := mustB64(f[1])
	compressed = append([]byte{2 + compressed[64]&1}, compressed[1:33]...)
	offCurve := mustB64(f[1])
	offCurve[64] ^= 1
	var bad []ControllerQRCase
	for _, c := range []ControllerQRCase{
		{Description: "Wrong prefix (version 2).", QR: strings.Replace(s, "ff1-ctl:1|", "ff1-ctl:2|", 1)},
		{Description: "Too few fields (label missing).", QR: s[:strings.LastIndex(s, "|")]},
		{Description: "channelId shorter than 16 characters.", QR: with(0, "short")},
		{Description: "channelId longer than 64 characters.", QR: with(0, strings.Repeat("a", 65))},
		{Description: "channelId with a character outside [A-Za-z0-9_-].", QR: with(0, "7b0d6e4a.2f51.4c8e.9a43.a48fcc56c5d9")},
		{Description: "minterKey in compressed form.", QR: with(1, b64u(compressed))},
		{Description: "minterKey not on P-256.", QR: with(1, b64u(offCurve))},
		{Description: "sig.y not on the curve with sig.x.", QR: with(3, f[5])},
		{Description: "ps of 15 bytes.", QR: with(6, b64u(mustB64(f[6])[:15]))},
		{Description: "ps with base64 padding.", QR: with(6, f[6]+"==")},
		{Description: "Unknown platform.", QR: with(7, "watch")},
		{Description: "Label with a control character.", QR: with(8, "bad\u0007label")},
		{Description: "Empty label.", QR: with(8, "")},
	} {
		if _, err := control.ParseControllerQR(c.QR); err == nil {
			return fmt.Errorf("controller QR %q accepted", c.Description)
		}
		bad = append(bad, c)
	}
	g.files["negative/controller-qr.json"] = Encode(ControllerQRVectors{Name: "controller-qr", Kind: "controllerQR",
		Description: "Controller QR payloads a parser MUST reject (§5.6.1).", Cases: bad})
	return nil
}
