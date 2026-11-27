package fixturegen

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// ClaimQR is claim_qr.json: one claim-QR link as the device paints it (§11)
// and everything a controller must read back from it. A controller's parser
// passes when it derives Profile from URL alone and its thumbprints equal
// Pins.
type ClaimQR struct {
	Description string `json:"description"`
	// URL is the QR payload: the fixed prefix and fourteen '|' fields.
	URL string `json:"url"`
	// Fields are the fourteen fields in order, for a parser to compare its
	// split against. Field 3 (branch) is shown as it appears in the URL.
	Fields     []string        `json:"fields"`
	Invitation string          `json:"invitation"`
	ExpiresAt  int64           `json:"expiresAt"`
	Host       string          `json:"host"`
	Pins       control.Pins    `json:"pins"`
	Profile    json.RawMessage `json:"profile"`
}

// The non-control inputs of the fixture link. The branch has a '/' so the
// one escape the builder applies is in the vector.
const (
	claimQRPrefix  = "https://link.feralfile.com/device_connect/"
	claimQRTopic   = "topic-fixture"
	claimQRBranch  = "feature%2Fclaim"
	claimQRVersion = "3.1.0"
	claimQRPhase   = "pairing"
	claimQRHost    = "192.168.1.23"
)

func (g *gen) claimQR() error {
	setup := g.setup("claim_qr")
	setup.Grants, setup.Invitation = []string{}, true
	a, _, err := BuildDevice(g.k, *setup)
	if err != nil {
		return err
	}
	inv, live := a.CurrentInvitation()
	if !live {
		return errors.New("claim_qr: no invitation")
	}
	fields := []string{DeviceID, claimQRTopic, "true", claimQRBranch, claimQRVersion, claimQRPhase,
		inv.Value, inv.Pins.Sig, inv.Pins.Enc, inv.Gen}
	fields = append(fields, inv.KeyFields(claimQRHost)...)
	g.files["claim_qr.json"] = Encode(ClaimQR{
		Description: "Claim QR of a legacy device that dials relay v2: fields 0-9, then exp, the compressed SEC 1 device keys and the LAN host (§11)",
		URL:         claimQRPrefix + strings.Join(fields, "|"),
		Fields:      fields, Invitation: inv.Value, ExpiresAt: inv.ExpiresAt, Host: claimQRHost,
		Pins: inv.Pins, Profile: g.k.Device.Profile,
	})
	return nil
}
