package playerlink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Snapshot is host state, not an artist document's self-report. Playlist and
// Item remain DP-1 documents: the transport must not discard extension fields.
// Selected describes intent; Presented describes the last composed occurrence.
type RuntimeNotice struct {
	Occurrence string `json:"occurrence"`
	Category   string `json:"category"`
	Message    string `json:"message"`
}

type Snapshot struct {
	Notice               *RuntimeNotice       `json:"notice,omitempty"`
	Engine               json.RawMessage      `json:"engine,omitempty"`
	Instance             string               `json:"instance"`
	Generation           uint64               `json:"generation"`
	Revision             uint64               `json:"revision"`
	Host                 string               `json:"host"`
	Renderer             string               `json:"renderer"`
	Selected             *Occurrence          `json:"selected"`
	Presented            *Occurrence          `json:"presented"`
	Composition          *Composition         `json:"composition"`
	Playlist             json.RawMessage      `json:"playlist"`
	PlaylistURL          string               `json:"playlistUrl"`
	PresentedPlaylist    json.RawMessage      `json:"presentedPlaylist"`
	PresentedPlaylistURL string               `json:"presentedPlaylistUrl"`
	Context              string               `json:"context"`
	Stopped              bool                 `json:"stopped"`
	Sleeping             bool                 `json:"sleeping"`
	Paused               bool                 `json:"paused"`
	MediaPaused          bool                 `json:"mediaPaused"`
	Loop                 string               `json:"loop"`
	Shuffle              bool                 `json:"shuffle"`
	Online               bool                 `json:"online"`
	Settings             Settings             `json:"settings"`
	Overlay              *Overlay             `json:"overlay"`
	Media                json.RawMessage      `json:"media"`
	Storage              StorageStatus        `json:"storage"`
	Recovery             *RecoveryStatus      `json:"recovery,omitempty"`
	PresentationFailure  *PresentationFailure `json:"presentationFailure,omitempty"`
	ScheduleOutcome      *ScheduleOutcome     `json:"scheduleOutcome"`
	PendingUpdate        *PendingUpdate       `json:"pendingUpdate"`
	UpdateOutcome        *UpdateOutcome       `json:"updateOutcome"`
}

// PendingUpdate identifies a durable source refresh waiting for a slot boundary.
// Its item count describes the replacement, not the currently presented list.
type PendingUpdate struct {
	ID        string `json:"id"`
	ItemCount int    `json:"itemCount"`
}

type UpdateOutcome struct {
	ID             string `json:"id"`
	Outcome        string `json:"outcome"`
	ContentBlocked bool   `json:"contentBlocked"`
}

// PresentationFailure belongs to a retired outgoing occurrence. It must not
// be attributed to the replacement that is still loading.
type PresentationFailure struct {
	Occurrence string `json:"occurrence"`
	Generation uint64 `json:"generation"`
	Error      string `json:"error"`
}

// RecoveryStatus describes the host's bounded renderer retries. RetryDelayMS
// is the backoff chosen when the failure occurred, not a live countdown.
// The budget resets on explicit selection, or after 60 seconds presented.
type RecoveryStatus struct {
	Attempt          int    `json:"attempt"`
	Limit            int    `json:"limit"`
	Pending          bool   `json:"pending"`
	Exhausted        bool   `json:"exhausted"`
	LastError        string `json:"lastError"`
	FailedOccurrence string `json:"failedOccurrence"`
	RetryDelayMS     int    `json:"retryDelayMs"`
}

// ScheduleOutcome records execution, not rendering. Its instance/generation
// identify the execution that produced it, including after a host restart.
type ScheduleOutcome struct {
	ID           string       `json:"id"`
	AtMS         int64        `json:"atMs"`
	ExecutedAtMS int64        `json:"executedAtMs"`
	Instance     string       `json:"instance"`
	Generation   uint64       `json:"generation"`
	Outcome      string       `json:"outcome"`
	Error        *RemoteError `json:"error,omitempty"`
}

type StorageStatus struct {
	Health string `json:"health"`
	Error  string `json:"error"`
}

type Occurrence struct {
	ID                  string          `json:"id"`
	Generation          uint64          `json:"generation"`
	Index               int             `json:"index"`
	Item                json.RawMessage `json:"item"`
	Display             Display         `json:"display"`
	DisplayRevision     uint64          `json:"displayRevision"`
	Stage               string          `json:"stage"`
	Error               string          `json:"error"`
	NavigationCommitted bool            `json:"navigationCommitted"`
	Context             string          `json:"context"`
	Exhibition          string          `json:"exhibition"`
	Curator             string          `json:"curator"`
}

// Composition is the last host-submitted configuration, distinct from desired
// occurrence settings while a renderer applies a newer configuration.
type Composition struct {
	Occurrence      string  `json:"occurrence"`
	Revision        uint64  `json:"revision"`
	DisplayRevision uint64  `json:"displayRevision"`
	Display         Display `json:"display"`
}

type Display struct {
	Scaling       string          `json:"scaling"`
	Margin        json.RawMessage `json:"margin"`
	Background    string          `json:"background"`
	AutoPlay      bool            `json:"autoPlay"`
	Loop          bool            `json:"loop"`
	UserOverrides bool            `json:"userOverrides"`
}

type Settings struct {
	Scaling         string   `json:"scaling"`
	Framing         string   `json:"framing"`
	Tombstone       string   `json:"tombstone"`
	TransitionMS    float64  `json:"transitionMs"`
	DurationSeconds *float64 `json:"durationSeconds"`
	Margin          float64  `json:"margin"`
	Background      string   `json:"background"`
	Rotation        int      `json:"rotation"`
	Volume          float64  `json:"volume"`
	Muted           bool     `json:"muted"`
}

type Overlay struct {
	Owner  string         `json:"owner"`
	State  string         `json:"state"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Receipt acknowledges host execution or admission, never physical output.
// Accepted operations must be followed through snapshots using Occurrence.
type Receipt struct {
	Outcome                     string  `json:"outcome"`
	Generation                  uint64  `json:"generation,omitempty"`
	Revision                    uint64  `json:"revision,omitempty"`
	Occurrence                  *string `json:"occurrence,omitempty"`
	AcceptedCompositionRevision *uint64 `json:"acceptedCompositionRevision,omitempty"`
}

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var state Snapshot
	if err := c.Call(ctx, "state.get", nil, &state, nil); err != nil {
		return Snapshot{}, err
	}
	if err := state.Validate(); err != nil {
		return Snapshot{}, err
	}
	return state, nil
}

// Validate rejects identity contradictions before they can authorize an input
// target or attach a trusted verdict. Additive unknown fields remain allowed.
func (s Snapshot) Validate() error {
	if s.Instance == "" || s.Generation == 0 || s.Revision == 0 || s.Host != "responsive" {
		return fmt.Errorf("native player returned invalid host identity/health")
	}
	for _, occurrence := range []*Occurrence{s.Selected, s.Presented} {
		if occurrence == nil {
			continue
		}
		if occurrence.ID == "" || occurrence.Generation == 0 || occurrence.Generation > s.Generation || occurrence.Index < 0 {
			return fmt.Errorf("native player returned invalid occurrence identity")
		}
	}
	// A deferred source refresh advances the command fence while the retained
	// selection still belongs to its earlier admitted document. Its generation
	// must never be replaced by the host's when attaching verification results.
	if s.Presented != nil && (s.Presented.Stage != "presented" || !s.Presented.NavigationCommitted || len(s.PresentedPlaylist) == 0 || string(s.PresentedPlaylist) == "null") {
		return fmt.Errorf("native player returned incomplete presentation state")
	}
	if s.Composition != nil && (s.Presented == nil || s.Composition.Occurrence != s.Presented.ID || s.Composition.Revision == 0 || s.Composition.DisplayRevision == 0 || s.Composition.DisplayRevision > s.Presented.DisplayRevision) {
		return fmt.Errorf("native player returned inconsistent composition identity")
	}
	return nil
}

func (s Snapshot) Preconditions() *Preconditions {
	return &Preconditions{Instance: s.Instance, Generation: s.Generation}
}

// PresentationURL reports device UI state, never an artist URL or credential.
// The step=qr hint preserves older phones' claim-QR visibility inference.
func (c *Client) PresentationURL(ctx context.Context) (string, error) {
	s, err := c.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	return s.PresentationURL(), nil
}

func (s Snapshot) PresentationURL() string {
	if s.Overlay != nil {
		u := url.URL{Scheme: "ff-player", Host: s.Overlay.Owner, Path: "/" + s.Overlay.State}
		if s.Overlay.Owner == "setup" && s.Overlay.State == "claim_qr" {
			u.RawQuery = "step=qr"
		}
		return u.String()
	}
	if s.Selected != nil && s.Selected.Stage == "failed" {
		return "ff-player://error/renderer"
	}
	if s.Sleeping {
		return "ff-player://sleep"
	}
	if s.Presented != nil {
		return "ff-player://artwork/" + url.PathEscape(s.Presented.ID)
	}
	if s.Selected != nil {
		return "ff-player://loading"
	}
	return "ff-player://idle"
}
