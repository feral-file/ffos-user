package status

import (
	"context"
	"encoding/json"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

// PlayerStatusSource keeps the external controller DTO at controld's boundary.
// The native protocol speaks playback state, never controller command envelopes.
type PlayerStatusSource interface {
	FetchPlayerStatus(context.Context) (*PlayerStatus, error)
}

type NativeSnapshotSource interface {
	Snapshot(context.Context) (playerlink.Snapshot, error)
}

type NativePlayer struct{ Source NativeSnapshotSource }

// RuntimeStatus is additive to the established controller status contract.
// It preserves admission/loading/failure information even while an outgoing
// occurrence remains visible. No artist-supplied health or trust fields enter it.
type RuntimeStatus struct {
	PresentedOwnsCast   *bool                           `json:"-"` // Internal refresh ownership from the native host.
	Notice              *playerlink.RuntimeNotice       `json:"notice,omitempty"`
	Media               json.RawMessage                 `json:"media,omitempty"`
	Engine              json.RawMessage                 `json:"engine,omitempty"`
	Instance            string                          `json:"instance"`
	Generation          uint64                          `json:"generation"`
	Host                string                          `json:"host"`
	Renderer            string                          `json:"renderer"`
	Stopped             bool                            `json:"stopped"`
	Sleeping            bool                            `json:"sleeping"`
	Storage             playerlink.StorageStatus        `json:"storage"`
	Recovery            *playerlink.RecoveryStatus      `json:"recovery,omitempty"`
	PresentationFailure *playerlink.PresentationFailure `json:"presentationFailure,omitempty"`
	Selected            *OccurrenceStatus               `json:"selected"`
	Presented           *OccurrenceStatus               `json:"presented"`
	ScheduleOutcome     *playerlink.ScheduleOutcome     `json:"scheduleOutcome,omitempty"`
	PendingUpdate       *playerlink.PendingUpdate       `json:"pendingUpdate,omitempty"`
	UpdateOutcome       *playerlink.UpdateOutcome       `json:"updateOutcome,omitempty"`
	MetadataError       string                          `json:"metadataError,omitempty"`
}

type OccurrenceStatus struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	Index      int    `json:"index"`
	Stage      string `json:"stage"`
	Error      string `json:"error,omitempty"`
}

func occurrenceStatus(o *playerlink.Occurrence) *OccurrenceStatus {
	if o == nil {
		return nil
	}
	return &OccurrenceStatus{ID: o.ID, Generation: o.Generation, Index: o.Index, Stage: o.Stage, Error: o.Error}
}

func (p *NativePlayer) FetchPlayerStatus(ctx context.Context) (*PlayerStatus, error) {
	s, err := p.Source.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return NativePlayerStatus(s)
}

// NativePlayerStatus preserves existing numeric render codes, but code 2 now
// requires a host-composed frame. It is not proof of artwork visual correctness.
func NativePlayerStatus(s playerlink.Snapshot) (*PlayerStatus, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	loop := LoopMode(s.Loop)
	paused := s.Paused || s.MediaPaused || s.Stopped || s.Sleeping
	out := &PlayerStatus{Ok: true, IsPaused: &paused, LoopMode: &loop, Shuffle: &s.Shuffle,
		Runtime: &RuntimeStatus{PresentedOwnsCast: s.PresentedOwnsCast, Notice: s.Notice, Media: s.Media, Engine: s.Engine, Instance: s.Instance, Generation: s.Generation, Host: s.Host, Renderer: s.Renderer, Stopped: s.Stopped, Sleeping: s.Sleeping, Storage: s.Storage,
			Selected: occurrenceStatus(s.Selected), Presented: occurrenceStatus(s.Presented), ScheduleOutcome: s.ScheduleOutcome, PendingUpdate: s.PendingUpdate, UpdateOutcome: s.UpdateOutcome, Recovery: s.Recovery, PresentationFailure: s.PresentationFailure},
		DeviceSettings: &PlayerDeviceSettings{Framing: &s.Settings.Framing, Tombstone: &s.Settings.Tombstone, DefaultDuration: s.Settings.DurationSeconds}}
	// Global intent remains readable when stopped/asleep, but it must not create
	// a showing key or manufacture successful rendering after restart.
	document, playlistURL, occurrence := s.Playlist, s.PlaylistURL, s.Selected
	out.ContentContext = s.Context
	if s.Presented != nil {
		document, playlistURL, occurrence = s.PresentedPlaylist, s.PresentedPlaylistURL, s.Presented
		out.ContentContext = occurrence.Context
		out.DeviceSettings.ShowingKey = &occurrence.ID
		out.DeviceSettings.Margin = occurrence.Display.Margin
		out.DeviceSettings.Background = &occurrence.Display.Background
		out.DeviceSettings.Scaling = &occurrence.Display.Scaling
		if s.Composition != nil {
			out.DeviceSettings.CompositionRevision = &s.Composition.Revision
			out.DeviceSettings.Margin = s.Composition.Display.Margin
			out.DeviceSettings.Background = &s.Composition.Display.Background
			out.DeviceSettings.Scaling = &s.Composition.Display.Scaling
		}
	}
	if len(document) > 0 && string(document) != "null" {
		out.Playlist = &dp1.Playlist{}
		if err := json.Unmarshal(document, out.Playlist); err != nil {
			// A schema mismatch must remain observable without turning a live
			// host into an apparent socket outage. Do not expose a partial typed
			// playlist that other consumers might mistake for valid source data.
			out.Playlist = nil
			out.Ok = false
			out.Runtime.MetadataError = "Native playlist metadata is incompatible with this controller"
			out.Error = &out.Runtime.MetadataError
		} else {
			out.Items = &out.Playlist.Items
			out.Command = "displayPlaylist"
		}
	}
	if playlistURL != "" {
		out.PlaylistURL = &playlistURL
	}
	if occurrence != nil {
		out.Index = &occurrence.Index
		render := 1
		switch occurrence.Stage {
		case "presented":
			render = 2
		case "failed":
			render = 3
			out.Error = &occurrence.Error
		}
		out.RenderStatus = &render
	}
	if s.Notice != nil {
		out.Error = &s.Notice.Message
	}
	return out, nil
}
