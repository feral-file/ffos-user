package status

// player_status's controld-owned seriesPlayback annotation
// (feral-file/ffos-user#425 follow-up): the device's current full/first/
// random override, surfaced passively on every player_status push rather
// than only in reply to an explicit getSeriesPlayback query. Same
// drop-then-set contract as Overlay/SignatureStatus, reusing their test
// harness.

import (
	"context"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

func TestPollPlayerStatus_AttachesSeriesPlaybackOverride(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	p.SetSeriesPlaybackSource(func() *dp1.SeriesPlaybackMode {
		mode := dp1.SeriesPlaybackRandom
		return &mode
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.DeviceSettings == nil || got.DeviceSettings.SeriesPlayback == nil {
		t.Fatal("expected seriesPlayback to be attached")
	}
	if *got.DeviceSettings.SeriesPlayback != dp1.SeriesPlaybackRandom {
		t.Fatalf("seriesPlayback = %v, want random", *got.DeviceSettings.SeriesPlayback)
	}
}

// No device override set (source returns nil): the field must be omitted,
// not reported as some zero value — nil distinguishes "follow the
// document" from any real mode.
func TestPollPlayerStatus_OmitsSeriesPlaybackWhenNoOverrideIsSet(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
		"deviceSettings": map[string]any{
			"scaling": "fit",
		},
	}, ws)
	p.SetSeriesPlaybackSource(func() *dp1.SeriesPlaybackMode { return nil })

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.DeviceSettings == nil {
		t.Fatal("expected deviceSettings to survive (it carries other fields)")
	}
	if got.DeviceSettings.SeriesPlayback != nil {
		t.Fatalf("expected no seriesPlayback override, got %v", *got.DeviceSettings.SeriesPlayback)
	}
}

// Unwired source (nil, same as a build that has not reached this wiring
// yet): must behave exactly like "no override" rather than panicking.
func TestPollPlayerStatus_SeriesPlaybackSourceUnwired(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.DeviceSettings != nil && got.DeviceSettings.SeriesPlayback != nil {
		t.Fatalf("expected no seriesPlayback override, got %v", *got.DeviceSettings.SeriesPlayback)
	}
}

// The field is controld-owned: a reply that already carries a
// player-supplied (or spoofed) deviceSettings.seriesPlayback must have it
// dropped and replaced by the wired source's own answer, never trust what
// arrived on the wire.
func TestPollPlayerStatus_DropsPlayerSuppliedSeriesPlaybackBeforeReannotating(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
		"deviceSettings": map[string]any{
			"seriesPlayback": "full",
		},
	}, ws)
	p.SetSeriesPlaybackSource(func() *dp1.SeriesPlaybackMode {
		mode := dp1.SeriesPlaybackFirst
		return &mode
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.DeviceSettings == nil || got.DeviceSettings.SeriesPlayback == nil {
		t.Fatal("expected seriesPlayback to be attached from the wired source")
	}
	if *got.DeviceSettings.SeriesPlayback != dp1.SeriesPlaybackFirst {
		t.Fatalf("seriesPlayback = %v, want first (the wired source's answer, not the player-supplied value)", *got.DeviceSettings.SeriesPlayback)
	}
}

// deviceSettings may be absent entirely from a round's reply (every one of
// its OTHER sub-fields was also unset) — a real device override must not
// be dropped just because of that.
func TestPollPlayerStatus_AttachesSeriesPlaybackWhenDeviceSettingsOtherwiseAbsent(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	p.SetSeriesPlaybackSource(func() *dp1.SeriesPlaybackMode {
		mode := dp1.SeriesPlaybackFull
		return &mode
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.DeviceSettings == nil || got.DeviceSettings.SeriesPlayback == nil {
		t.Fatal("expected a freshly allocated deviceSettings carrying seriesPlayback")
	}
	if *got.DeviceSettings.SeriesPlayback != dp1.SeriesPlaybackFull {
		t.Fatalf("seriesPlayback = %v, want full", *got.DeviceSettings.SeriesPlayback)
	}
}
