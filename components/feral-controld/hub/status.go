package hub

import (
	"context"
	"net/http"
	"strings"

	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// StatusContract is the LAN API version reported by GET /api/status. It is a
// forward-compatibility detection field for the #3471 dual-running window: while
// BLE and the new LAN pairing path co-exist, a client reads this to tell which
// LAN contract a given device speaks before it commits to a flow. Bump this only
// on a breaking change to the LAN API surface; additive fields keep "1".
const StatusContract = "1"

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
	Claimed  bool
	// SetupState is a coarse provisioning-state string. Today it is a
	// placeholder derived from claim state; the provisioning package will
	// replace it with a real signal.
	SetupState string
	// Connectivity is a coarse link-state string (e.g. "connected").
	Connectivity string
	// TopicID is the relayer topicID. It is the transitional LAN topic-handover
	// value replacing BLE's and is expected to be dropped in LAN contract v2.
	// handleStatus serves it ONLY while the device is unclaimed (the claim
	// handover is its sole purpose); providers should still populate it
	// unconditionally and let the transport own that wire-level policy.
	TopicID string
}

// statusResponse is the on-the-wire JSON shape of GET /api/status.
type statusResponse struct {
	DeviceID     string `json:"device_id"`
	Version      string `json:"version"`
	Contract     string `json:"contract"`
	Claimed      bool   `json:"claimed"`
	SetupState   string `json:"setup_state"`
	Connectivity string `json:"connectivity"`
	TopicID      string `json:"topic_id"`
}

// handleStatus serves GET /api/status: a small JSON snapshot LAN clients use to
// discover a device's identity, LAN contract version, and claim/setup state.
func (h *hub) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var info StatusInfo
	if h.statusProvider != nil {
		info = h.statusProvider.Status(r.Context())
	}

	// topic_id is the LAN replacement for the value BLE used to hand over a
	// PRIVATE pairing link, and it is the relayer routing key that reaches the
	// device from anywhere. It exists on this endpoint solely so an unclaimed
	// device can be claimed over LAN; once the device is claimed there is no
	// legitimate unauthenticated LAN reader, and serving it would hand any LAN
	// peer the cloud-side command topic of an owned device. Withhold it the
	// moment the device is claimed. (The ff-app claim flow independently rejects
	// claimed==true — this is the device-side half of that same guard.)
	topicID := info.TopicID
	if info.Claimed {
		topicID = ""
	}

	resp := statusResponse{
		DeviceID:     info.DeviceID,
		Version:      info.Version,
		Contract:     StatusContract,
		Claimed:      info.Claimed,
		SetupState:   info.SetupState,
		Connectivity: info.Connectivity,
		TopicID:      topicID,
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
	s := state.GetState()

	deviceID := p.deviceID(s)
	claimed := s.ConnectedDevice != nil && strings.TrimSpace(s.ConnectedDevice.ID) != ""

	topicID := ""
	if s.Relayer != nil {
		topicID = s.Relayer.TopicID
	}

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

	return StatusInfo{
		DeviceID:     deviceID,
		Version:      p.installedVersion(),
		Claimed:      claimed,
		SetupState:   setupState,
		Connectivity: connectivity,
		TopicID:      topicID,
	}
}

// deviceID prefers the hostname (the identity mDNS also advertises) and falls
// back to the connected-device ID from persisted state.
func (p *stateStatusProvider) deviceID(s *state.State) string {
	if hostnameBytes, err := p.os.ReadFile(constants.HOSTNAME_FILE); err == nil {
		if hostname := strings.TrimSpace(string(hostnameBytes)); hostname != "" {
			return hostname
		}
	}
	if s.ConnectedDevice != nil {
		return strings.TrimSpace(s.ConnectedDevice.ID)
	}
	return ""
}

// installedVersion reads the installed version from the FF1 config file. It is
// best-effort: any read/parse failure yields an empty version rather than
// failing the whole status response.
func (p *stateStatusProvider) installedVersion() string {
	configBytes, err := p.os.ReadFile(constants.FF1_CONFIG_FILE)
	if err != nil {
		return ""
	}
	var cfg struct {
		Version string `json:"version"`
	}
	if err := p.json.Unmarshal(configBytes, &cfg); err != nil {
		return ""
	}
	return cfg.Version
}
