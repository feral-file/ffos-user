package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"time"

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

// mediaHealthCursorReconciler flags the outcome-ring cursor on every
// player-session generation. A generation can be a new page (numbering
// restarts) or a CDP reconnect to the surviving page (the session cannot
// tell: it re-stamps the document either way), so the service decides
// from the ring itself which it was (see mediahealth.Drainer).
func mediaHealthCursorReconciler(svc *mediahealth.Service) func(context.Context) {
	return func(context.Context) { svc.ResetOutcomeCursor() }
}

// wireMediaHealthL1 builds the nightly render runner (docs/media-health.md
// §6) when mediaHealth.l1.enabled is set and the offline cache's renderer
// exists; nil otherwise, with one log line saying why, so a device whose
// operator expected L1 can see from the journal that it is off.
func wireMediaHealthL1(cfg config.MediaHealthConfig, store *mediahealth.Store, renderer mediahealth.Renderer, rewriter *uarewrite.Policy, asleep, admit func() bool, logger *zap.Logger) *mediahealth.L1 {
	if cfg.L1 == nil || !cfg.L1.Enabled {
		logger.Info("media health: L1 render probe off (mediaHealth.l1.enabled is not set)")
		return nil
	}
	if renderer == nil {
		logger.Warn("media health: L1 render probe enabled but no headless renderer (offline cache disabled); not running")
		return nil
	}
	var known []mediahealth.Fingerprint
	for _, fp := range cfg.L1.KnownBad {
		parsed, err := mediahealth.ParseFingerprint(fp.PHash, fp.MaxDistance, fp.Label)
		if err != nil {
			logger.Warn("media health: dropping unparsable L1 fingerprint", zap.Error(err))
			continue
		}
		known = append(known, parsed)
	}
	l1cfg := mediahealth.L1Config{
		Store:                  store,
		Renderer:               renderer,
		Asleep:                 asleep,
		Admit:                  admit,
		Logger:                 logger,
		MaxPerNight:            cfg.L1.MaxPerNight,
		MinAsleep:              time.Duration(cfg.L1.MinAsleepSeconds) * time.Second,
		BlankVarianceThreshold: cfg.L1.BlankVarianceThreshold,
		KnownBad:               known,
		Settle:                 time.Duration(cfg.L1.SettleMs) * time.Millisecond,
		ConfirmSettle:          time.Duration(cfg.L1.ConfirmSettleMs) * time.Millisecond,
	}
	// Same typed-nil guard as wireMediaHealth: a nil *Policy must never
	// land in the interface field.
	if rewriter != nil {
		l1cfg.Rewriter = rewriter
	}
	l1 := mediahealth.NewL1(l1cfg)
	logger.Info("media health: L1 render probe on",
		zap.Int("max_per_night", cfg.L1.MaxPerNight),
		zap.Int("min_asleep_seconds", cfg.L1.MinAsleepSeconds),
		zap.Int("known_bad_fingerprints", len(known)),
		zap.Bool("admission_gate", admit != nil))
	return l1
}
