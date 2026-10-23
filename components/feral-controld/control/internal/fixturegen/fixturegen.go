// Package fixturegen builds the cross-language conformance vectors of
// docs/ff1-control-v1.md §12. Everything is derived from fixed labels, so a
// regeneration is byte-identical; control's golden test enforces that the
// committed files match what the current code produces.
package fixturegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

const (
	// Now is the fixed clock of every vector (2026-01-01T00:00:00Z).
	Now = 1_767_225_600
	// DeviceID is the fixture device's hostname.
	DeviceID = "FF1-FIXTURE1"
	// Dir is where BuildDevice provisions the in-memory control dir.
	Dir = "/fixture/control"
)

// Keys is keys.json.
type Keys struct {
	V           int                       `json:"v"`
	Now         int64                     `json:"now"`
	Device      DeviceKeys                `json:"device"`
	Controllers map[string]ControllerKeys `json:"controllers"`
}

// DeviceKeys holds the fixture device identity.
type DeviceKeys struct {
	Dev     string          `json:"dev"`
	Gen     string          `json:"gen"`
	Sig     PrivateJWK      `json:"sig"`
	Enc     PrivateJWK      `json:"enc"`
	Profile json.RawMessage `json:"profile"`
	Pins    control.Pins    `json:"pins"`
}

// ControllerKeys is one fixture controller. Role is empty for a controller
// that has no grant (the "stranger").
type ControllerKeys struct {
	Ctl       string     `json:"ctl"`
	Sig       PrivateJWK `json:"sig"`
	Enc       PrivateJWK `json:"enc"`
	Label     string     `json:"label"`
	Platform  string     `json:"platform"`
	Role      string     `json:"role,omitempty"`
	NK        string     `json:"nk,omitempty"`
	CreatedAt int64      `json:"createdAt,omitempty"`
	RevokedAt int64      `json:"revokedAt,omitempty"`
}

// Setup is the device state a request vector is opened against.
type Setup struct {
	Now int64 `json:"now"`
	// Grants lists the keys.json controllers present in grants.json.
	Grants []string `json:"grants"`
	// Windows overrides a grant's replay window.
	Windows map[string]WindowSpec `json:"windows,omitempty"`
	// Invitation: call IssueInvitation once after start (first rand draw).
	Invitation bool `json:"invitation,omitempty"`
	// RandSeed seeds the device's ChaCha8 stream (math/rand/v2), which
	// supplies reply IVs, nk and the invitation.
	RandSeed string `json:"randSeed"`
}

// WindowSpec is a §6 window in the grants.json encoding.
type WindowSpec struct {
	Hi     uint64 `json:"hi"`
	Bitmap string `json:"bitmap"`
}

// Fixture is one positive/ or negative/ file.
type Fixture struct {
	Name          string               `json:"name"`
	Kind          string               `json:"kind"` // request | reply | notification
	Description   string               `json:"description"`
	Setup         *Setup               `json:"setup,omitempty"`
	From          string               `json:"from,omitempty"`
	Payload       string               `json:"payload,omitempty"`
	Claims        json.RawMessage      `json:"claims,omitempty"`
	EphemeralKey  *PrivateJWK          `json:"ephemeralKey,omitempty"`
	IV            string               `json:"iv,omitempty"`
	JWS           string               `json:"jws,omitempty"`
	Compact       string               `json:"compact,omitempty"`
	CompactRepeat *Repeat              `json:"compactRepeat,omitempty"`
	RK            string               `json:"rk,omitempty"`
	To            string               `json:"to,omitempty"`
	NK            string               `json:"nk,omitempty"`
	Bind          *Bind                `json:"bind,omitempty"`
	Expect        Expect               `json:"expect"`
	Reply         *ReplyVector         `json:"reply,omitempty"`
	Notifications []NotificationVector `json:"notifications,omitempty"`
}

// Repeat describes a compact too large to commit: Char repeated Count times.
type Repeat struct {
	Char  string `json:"char"`
	Count int    `json:"count"`
}

// Bind is what a controller compares a reply against (§4.2).
type Bind struct {
	Dev string `json:"dev"`
	Gen string `json:"gen"`
	Ctl string `json:"ctl"`
	Rid string `json:"rid"`
}

// Expect is the asserted outcome.
type Expect struct {
	// Request vectors.
	Execute   bool               `json:"execute,omitempty"`
	Command   string             `json:"command,omitempty"`
	Principal *control.Principal `json:"principal,omitempty"`
	Public    string             `json:"public,omitempty"`
	ReplyOK   bool               `json:"replyOk,omitempty"`
	ReplyCode string             `json:"replyCode,omitempty"`
	// Reply/notification vectors: the client MUST reject.
	Reject bool `json:"reject,omitempty"`
}

// ReplyVector is the device's answer to a request vector.
type ReplyVector struct {
	Compact string          `json:"compact"`
	Claims  json.RawMessage `json:"claims"`
}

// NotificationVector is a sealed notification and its plaintext.
type NotificationVector struct {
	To      string          `json:"to"`
	Compact string          `json:"compact"`
	Claims  json.RawMessage `json:"claims"`
	Wire    string          `json:"wire,omitempty"`
}

// NewKeys builds keys.json.
func NewKeys() Keys {
	sig, enc := jwkFromScalar(scalar("device/sig")), jwkFromScalar(scalar("device/enc"))
	gen := b64u(derive("device/gen")[:16])
	id, err := control.NewIdentityFromKeys(DeviceID, gen, sig.ECDSA(), enc.ECDH(), Now-86400)
	if err != nil {
		panic(err)
	}
	profile, _ := json.Marshal(id.Profile())
	k := Keys{V: 1, Now: Now, Device: DeviceKeys{Dev: DeviceID, Gen: gen, Sig: sig, Enc: enc, Profile: profile, Pins: id.Pins()},
		Controllers: map[string]ControllerKeys{}}
	add := func(name, label, platform, role string, revokedAt int64) {
		c := ControllerKeys{Sig: jwkFromScalar(scalar("ctl/" + name + "/sig")), Enc: jwkFromScalar(scalar("ctl/" + name + "/enc")),
			Label: label, Platform: platform, Role: role, RevokedAt: revokedAt}
		c.Ctl = c.Sig.Thumb()
		if role != "" {
			c.NK = b64u(derive("ctl/" + name + "/nk"))
			c.CreatedAt = Now - 3600
		}
		k.Controllers[name] = c
	}
	add("owner", "Owner phone", "ios", "owner", 0)
	add("controller", "Living room tablet", "android", "controller", 0)
	add("revoked", "Old laptop", "cli", "controller", Now-600)
	add("stranger", "Stranger", "other", "", 0)
	return k
}

func (k Keys) identity() *control.Identity {
	id, err := control.NewIdentityFromKeys(k.Device.Dev, k.Device.Gen, k.Device.Sig.ECDSA(), k.Device.Enc.ECDH(), Now-86400)
	if err != nil {
		panic(err)
	}
	return id
}

func pub(j PrivateJWK) control.PublicJWK {
	p, err := control.ParsePublicJWK([]byte(j.PublicJSON()))
	if err != nil {
		panic(err)
	}
	return p
}

// BuildDevice provisions an in-memory device for setup and opens its
// Authority. The returned invitation is set when setup.Invitation is.
func BuildDevice(k Keys, s Setup) (*control.Authority, string, error) {
	fsys := control.NewMemFS()
	var grants []control.Grant
	for _, name := range s.Grants {
		c, ok := k.Controllers[name]
		if !ok || c.Role == "" {
			return nil, "", fmt.Errorf("setup grant %q has no role in keys.json", name)
		}
		g := control.Grant{Ctl: c.Ctl, Sig: pub(c.Sig), Enc: pub(c.Enc), Role: control.Role(c.Role), Label: c.Label,
			Platform: c.Platform, CreatedAt: c.CreatedAt, ApprovedBy: "invitation", RevokedAt: c.RevokedAt}
		copy(g.NK[:], mustB64(c.NK))
		if c.Role != "owner" {
			g.ApprovedBy = k.Controllers["owner"].Ctl
		}
		if w, ok := s.Windows[name]; ok {
			bm, err := strconv.ParseUint(w.Bitmap, 16, 64)
			if err != nil {
				return nil, "", err
			}
			g.Window = control.Window{Hi: w.Hi, Bitmap: bm}
		}
		grants = append(grants, g)
	}
	if err := control.InitDir(fsys, Dir, k.identity(), grants); err != nil {
		return nil, "", err
	}
	a, err := control.New(control.Config{Dir: Dir, DeviceID: k.Device.Dev, FS: fsys,
		Now:  func() time.Time { return time.Unix(s.Now, 0) },
		Rand: rand.NewChaCha8([32]byte(mustB64(s.RandSeed)))})
	if err != nil {
		return nil, "", err
	}
	if err := a.LockErr(); err != nil {
		return nil, "", err
	}
	var inv string
	if s.Invitation {
		i, err := a.IssueInvitation()
		if err != nil {
			return nil, "", err
		}
		inv = i.Value
	}
	return a, inv, nil
}

// Encode renders a vector file: indented, no HTML escaping, final newline.
func Encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
