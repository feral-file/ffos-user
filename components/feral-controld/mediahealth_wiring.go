package main

import (
	"context"
	"encoding/json"
	"net"
	"os"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
	"github.com/feral-file/ffos-user/components/feral-controld/uarewrite"
)

// mediaHealthMinFreeBytes is the free-space reserve below which the media
// observation store refuses writes: the offline cache's own default reserve
// (docs/automatic-playback-cache.md: keep the larger of 5 GiB or 10% free),
// so the two never fight over the last gigabytes. The store never evicts,
// so this guard is the only thing that keeps it from being the writer that
// fills the disk.
const mediaHealthMinFreeBytes = offlinecache.DefaultMinFreeDiskBytes

// wireMediaHealth builds the media observation service
// (feral-file/ffos-user#392, docs/media-health.md): the bbolt store, the
// L0 probe over the offline cache's guarded fetcher, the cast-time pass,
// and the during-play lookahead. nil when disabled or when the store cannot
// be opened — the daemon then runs exactly as before #392: casts forwarded
// unprobed, no hints, no per-item status.
//
// rewriter is the kiosk's gateway policy, so the probe sees the same wire
// the player fetches (ipfs.io/dweb.link content-addressed paths go to the
// replacement gateway; other paths on those hosts get the policy agent).
// A nil *uarewrite.Policy must NOT be assigned to the interface field — a
// typed nil would pass the nil check inside the probe — hence the guard.
//
// online is the device's applied internet verdict (the mediator's
// connectivity edges, see main): play failures observed while it is false
// are recorded but never counted, so an outage cannot mark the wall broken.
func wireMediaHealth(cfg config.MediaHealthConfig, rewriter *uarewrite.Policy, online func() bool, replayable func(string) bool, logger *zap.Logger) (*mediahealth.Service, *mediahealth.Lookahead) {
	if cfg.Disabled {
		logger.Warn("media health disabled by config; casts are forwarded unprobed and carry no hints")
		return nil, nil
	}
	store, err := mediahealth.Open(constants.MEDIA_HEALTH_STORE_FILE, mediahealth.Options{
		Build:        mediahealth.Build{Controld: installedControldVersion()},
		MinFreeBytes: mediaHealthMinFreeBytes,
		Logger:       logger,
	})
	if err != nil {
		// Not fatal: this daemon owns every recovery surface on the device,
		// and a store that will not open must cost observations, never the
		// wall. Restart=always retries on the next start.
		logger.Error("media health store unavailable; running without media observations", zap.Error(err))
		return nil, nil
	}
	maxBytes := cfg.ProbeMaxBytes
	if maxBytes <= 0 {
		maxBytes = mediahealth.DefaultProbeMaxBytes
	}
	probe := &mediahealth.Probe{
		Fetcher:   offlinecache.NewMediaHealthFetcher(net.DefaultResolver),
		Validator: mediahealth.NewContentValidator(maxBytes, cfg.KnownBadPageMarkers),
		MaxBytes:  maxBytes,
	}
	if rewriter != nil {
		probe.Rewriter = rewriter
	}
	prober := &mediahealth.Prober{Probe: probe, Store: store, Logger: logger}
	var lookahead *mediahealth.Lookahead
	if cfg.Lookahead >= 0 {
		lookahead = &mediahealth.Lookahead{Prober: prober, Store: store, Logger: logger, Window: cfg.Lookahead}
	}
	svc := mediahealth.New(mediahealth.Config{
		Store:      store,
		Prober:     prober,
		Contract:   &mediahealth.PlayerContract{Path: setupui.DefaultContractPath},
		Lookahead:  lookahead,
		Online:     online,
		Replayable: replayable,
		Logger:     logger,
	})
	count, _ := store.Count()
	logger.Info("media health store opened",
		zap.String("path", constants.MEDIA_HEALTH_STORE_FILE),
		zap.Int("records", count),
		zap.Int("probe_max_bytes", maxBytes),
		zap.Bool("lookahead", lookahead != nil),
		zap.Bool("rewriter", rewriter != nil))
	return svc, lookahead
}

// installedControldVersion reads the installed version from the FF1 config
// file (the same source device status reports as installedVersion). Best
// effort: "" when unreadable — the build stamp on a record is diagnostic,
// never a gate.
func installedControldVersion() string {
	raw, err := os.ReadFile(constants.FF1_CONFIG_FILE)
	if err != nil {
		return ""
	}
	var cfg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	return cfg.Version
}

// mediaHealthCursorReconciler forgets the outcome-ring cursor on every
// player-session generation: a new page restarts the ring's numbering, so
// nothing in the fresh ring was seen before (see mediahealth.Drainer).
func mediaHealthCursorReconciler(svc *mediahealth.Service) func(context.Context) {
	return func(context.Context) { svc.ResetOutcomeCursor() }
}
