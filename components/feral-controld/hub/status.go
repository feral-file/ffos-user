package hub

import (
	"context"
	"net/http"
	"strings"

	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// StatusContract is the LEGACY LAN API version reported by GET /api/status.
// The route is kept for transitional tooling that already reads it; it is NOT
// the surface the pairing app keys on (see StatusContractV2). Bump neither in
// place: breaking changes get a new versioned route.
const StatusContract = "1"

// StatusContractV2 is the LAN API version reported by GET /api/v2/status — the
// pairing surface the new app keys on. The versioned route is the firmware
// gate: old firmware serves no /api/v2/status (and no `api` mDNS TXT key), so
// the app can tell a LAN-pairable device from an old one by a 404 alone,
// without shape-sniffing /api/status. Keep in sync with mdns' advertised
// api=<v> TXT value.
const StatusContractV2 = "2"

// StatusProvider supplies the dynamic device fields served at GET /api/status.
// The hub owns only the transport and the contract version; everything device-
// specific comes through this seam. It is the injection point the forthcoming
// provisioning package will implement (feeding a real setup_state in
// particular). A nil provider is tolerated and yields a minimal response
// carrying just the contract.
type StatusProvider interface {
	Status(ctx context.Context) StatusInfo
}

// StatusInfo is the device-specific half of the /api/status payload.
type StatusInfo struct {
	DeviceID string
	Version  string
	// Branch is the running build's distribution branch, read from the same
	// ff1-config.json the OTA gate uses. It exists for claim-QR parity: the
	// device_connect QR encodes device_id|topic_id|internet|branch|version|
	// phase, and the LAN claim path must expose the same facts.
	Branch  string
	Claimed bool
	// Internet reports live internet reachability (the sys-monitord signal) —
	// the claim-QR "internet" segment. Distinct from Connectivity, which is
	// LAN-link state: a device on a healthy LAN with a dead WAN is
	// connectivity=connected, internet=false.
	Internet bool
	// SetupState is a coarse provisioning-state string. Today it is a
	// placeholder derived from claim state; the provisioning package will
	// replace it with a real signal.
	SetupState string
	// Connectivity is a coarse link-state string (e.g. "connected").
	Connectivity string
	// TopicID is the relayer topicID. It is the transitional LAN topic-handover
	// value replacing BLE's and is expected to be dropped in LAN contract v2.
	// handleStatus serves it unconditionally, claimed or not: FF1 supports
	// multiple controlling phones, so a claimed device must stay pairable
	// from any phone on the LAN (see handleStatus for the trust-model note).
	TopicID string
	// Network is the §4.7 health object (nil when no provisioning machine is
	// wired — the additive field is then omitted).
	Network *status.NetworkHealth
}

// statusResponse is the on-the-wire JSON shape of GET /api/status AND
// /api/v2/status (identical fields; only the contract value differs per
// route). branch and internet are additive (the legacy route keeps contract
// "1"); together with the existing fields they make the LAN payload
// informationally equivalent to the
// device_connect claim QR (device_id|topic_id|internet|branch|version|phase —
// the QR's constant "pairing" phase is derivable as claimed==false with a
// served topic_id).
type statusResponse struct {
	DeviceID     string `json:"device_id"`
	Version      string `json:"version"`
	Branch       string `json:"branch"`
	Contract     string `json:"contract"`
	Claimed      bool   `json:"claimed"`
	Internet     bool   `json:"internet"`
	SetupState   string `json:"setup_state"`
	Connectivity string `json:"connectivity"`
	TopicID      string `json:"topic_id"`
	// network is the additive §4.7 health object (docs/network-recovery-ux.md);
	// omitted when no provisioning machine is wired.
	Network *status.NetworkHealth `json:"network,omitempty"`
}

// handleStatus serves GET /api/status — the legacy (contract "1") route, kept
// for transitional tooling. New clients use /api/v2/status.
func (h *hub) handleStatus(w http.ResponseWriter, r *http.Request) {
	h.serveStatus(w, r, StatusContract)
}

// handleStatusV2 serves GET /api/v2/status — the LAN pairing surface. Same
// payload shape as v1; the versioned route (plus the mDNS `api` TXT key) is
// what lets the app filter out old firmware.
func (h *hub) handleStatusV2(w http.ResponseWriter, r *http.Request) {
	h.serveStatus(w, r, StatusContractV2)
}

// serveStatus renders the status snapshot LAN clients use to discover a
// device's identity, LAN contract version, and claim/setup state.
func (h *hub) serveStatus(w http.ResponseWriter, r *http.Request, contract string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var info StatusInfo
	if h.statusProvider != nil {
		info = h.statusProvider.Status(r.Context())
	}

	// topic_id is served claimed or not. FF1 is a multi-controller product:
	// several phones control one frame, and a frame whose original phone was
	// lost or wiped must remain pairable from any phone on the LAN. The
	// app-side flow suppresses only the unprompted app-open claim offer for
	// claimed devices — manual pairing always works. LAN-presence acts as the
	// authorization boundary, matching the BLE-era posture (any nearby phone
	// could pair without authentication); an on-device confirmation before
	// topic handover is the LAN contract v2 path if this needs tightening.
	resp := statusResponse{
		DeviceID:     info.DeviceID,
		Version:      info.Version,
		Branch:       info.Branch,
		Contract:     contract,
		Claimed:      info.Claimed,
		Internet:     info.Internet,
		SetupState:   info.SetupState,
		Connectivity: info.Connectivity,
		TopicID:      info.TopicID,
		Network:      info.Network,
	}

	if err := h.respondJSON(w, http.StatusOK, resp); err != nil {
		h.logger.Warn("Failed to respond with status JSON", zap.Error(err))
	}
}

// linkReporter is the small local seam the default status provider uses to
// report connectivity, satisfied by *status.LinkChecker. Keeping it as a local
// interface means the hub does not import the status package's concrete type.
type linkReporter interface {
	HasLink(ctx context.Context) bool
}

// stateStatusProvider is the default StatusProvider. It reads device identity
// and version from the same on-disk sources the device-status collector uses,
// claim state and topicID from persisted state, and connectivity from the link
// seam. It carries no provisioning logic of its own — the provisioning package
// will supply a richer provider (and a real setup_state) later.
type stateStatusProvider struct {
	os     wrapper.OS
	json   wrapper.JSON
	link   linkReporter
	logger *zap.Logger
}

// NewStateStatusProvider builds the default StatusProvider from existing
// on-device state sources.
func NewStateStatusProvider(os wrapper.OS, json wrapper.JSON, link linkReporter, logger *zap.Logger) StatusProvider {
	return &stateStatusProvider{os: os, json: json, link: link, logger: logger}
}

func (p *stateStatusProvider) Status(ctx context.Context) StatusInfo {
	// Single snapshot: claimed-ness and the topic must come from the SAME
	// write, never two separate GetState() field reads straddling a
	// concurrent connect()/clearPersistedClaim()/topic assignment.
	claim := state.ClaimSnapshot()

	deviceID := p.deviceID(claim)
	claimed := claim.Claimed
	topicID := claim.TopicID

	connectivity := "disconnected"
	if p.link != nil && p.link.HasLink(ctx) {
		connectivity = "connected"
	}

	// setup_state placeholder: the only setup signal available today is whether
	// the device is claimed. The provisioning package will replace this.
	setupState := "unclaimed"
	if claimed {
		setupState = "claimed"
	}

	version, branch := p.installedBuild()
	return StatusInfo{
		DeviceID:     deviceID,
		Version:      version,
		Branch:       branch,
		Claimed:      claimed,
		SetupState:   setupState,
		Connectivity: connectivity,
		TopicID:      topicID,
	}
}

// deviceID prefers the hostname (the identity mDNS also advertises) and falls
// back to the connected-device ID from the claim snapshot.
func (p *stateStatusProvider) deviceID(claim state.ClaimInfo) string {
	if hostnameBytes, err := p.os.ReadFile(constants.HOSTNAME_FILE); err == nil {
		if hostname := strings.TrimSpace(string(hostnameBytes)); hostname != "" {
			return hostname
		}
	}
	return strings.TrimSpace(claim.DeviceID)
}

// installedBuild reads the installed version and distribution branch from the
// FF1 config file — the SAME source the OTA gate and the claim QR read, so the
// LAN payload can never disagree with the QR. Best-effort: any read/parse
// failure yields empty values rather than failing the whole status response.
func (p *stateStatusProvider) installedBuild() (version, branch string) {
	configBytes, err := p.os.ReadFile(constants.FF1_CONFIG_FILE)
	if err != nil {
		return "", ""
	}
	var cfg struct {
		Version string `json:"version"`
		Branch  string `json:"branch"`
	}
	if err := p.json.Unmarshal(configBytes, &cfg); err != nil {
		return "", ""
	}
	return cfg.Version, cfg.Branch
}
