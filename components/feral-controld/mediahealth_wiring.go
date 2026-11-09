package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
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
//
// pushHints, when non-nil, delivers the hint-only push (newHintSender); the
// returned pusher's Run must be driven by the caller, like the lookahead's.
//
// overrideActive says the kiosk interceptor runs and will load a work
// through the fallback gateway the probe found serving it.
func wireMediaHealth(cfg config.MediaHealthConfig, rewriter *uarewrite.Policy, overrideActive bool, online func() bool, replayable func(string) bool, pushHints mediahealth.HintSender, logger *zap.Logger) (*mediahealth.Service, *mediahealth.Lookahead, *mediahealth.HintPusher) {
	gateways := mediahealth.NewGateways(cfg.IPFSGateways, cfg.ArweaveGateways)
	if cfg.Disabled {
		logger.Warn("media health disabled by config; casts are forwarded unprobed and carry no hints")
		return nil, nil, nil
	}
	store, err := mediahealth.Open(constants.MEDIA_HEALTH_STORE_FILE, mediahealth.Options{
		Build:        mediahealth.Build{Controld: installedControldVersion()},
		Gateways:     gateways,
		MinFreeBytes: mediaHealthMinFreeBytes,
		Logger:       logger,
	})
	if err != nil {
		// Not fatal: this daemon owns every recovery surface on the device,
		// and a store that will not open must cost observations, never the
		// wall. Restart=always retries on the next start.
		logger.Error("media health store unavailable; running without media observations", zap.Error(err))
		return nil, nil, nil
	}
	maxBytes := cfg.ProbeMaxBytes
	if maxBytes <= 0 {
		maxBytes = mediahealth.DefaultProbeMaxBytes
	}
	probe := &mediahealth.Probe{
		Fetcher:        offlinecache.NewMediaHealthFetcher(net.DefaultResolver),
		Validator:      mediahealth.NewContentValidator(maxBytes, cfg.KnownBadPageMarkers),
		MaxBytes:       maxBytes,
		Gateways:       gateways,
		OverrideActive: overrideActive,
	}
	if rewriter != nil {
		probe.Rewriter = rewriter
	}
	// One HostLimiter for the pass and the lookahead: per-host pacing and
	// 429/503 cooldowns only work if both consumers see the same state.
	prober := &mediahealth.Prober{
		Probe:       probe,
		Store:       store,
		Logger:      logger,
		Hosts:       &mediahealth.HostLimiter{},
		Online:      online,
		Items:       cfg.CastItems,
		Concurrency: cfg.CastConcurrency,
	}
	var lookahead *mediahealth.Lookahead
	if cfg.Lookahead >= 0 {
		lookahead = &mediahealth.Lookahead{Prober: prober, Store: store, Logger: logger}
	}
	contract := &mediahealth.PlayerContract{Path: setupui.DefaultContractPath}
	var pusher *mediahealth.HintPusher
	if pushHints != nil {
		pusher = &mediahealth.HintPusher{Send: pushHints, Supported: contract.SupportsHintPush, Logger: logger}
	}
	svc := mediahealth.New(mediahealth.Config{
		Store:      store,
		Prober:     prober,
		Contract:   contract,
		Lookahead:  lookahead,
		Pusher:     pusher,
		Online:     online,
		Replayable: replayable,
		Logger:     logger,
	})
	count, _ := store.Count()
	logger.Info("media health store opened",
		zap.String("path", constants.MEDIA_HEALTH_STORE_FILE),
		zap.Int("records", count),
		zap.Int("probe_max_bytes", maxBytes),
		zap.Int("cast_items", cmp.Or(cfg.CastItems, mediahealth.DefaultPassItems)),
		zap.Int("cast_concurrency", cmp.Or(cfg.CastConcurrency, mediahealth.DefaultConcurrency)),
		zap.Bool("lookahead", lookahead != nil),
		zap.Bool("rewriter", rewriter != nil))
	if pusher != nil {
		// The Service computes the map exactly as every push does.
		pusher.Hints = svc.Hints
	}
	return svc, lookahead, pusher
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
func wireMediaHealthL1(cfg config.MediaHealthConfig, store *mediahealth.Store, renderer mediahealth.Renderer, asleepSince func() (time.Time, bool), admit func() bool, logger *zap.Logger) *mediahealth.L1 {
	if cfg.L1 == nil || !cfg.L1.Enabled {
		logger.Info("media health: L1 render probe off (mediaHealth.l1.enabled is not set)")
		return nil
	}
	if renderer == nil {
		logger.Warn("media health: L1 render probe enabled but no headless renderer (offline cache disabled); not running")
		return nil
	}
	known := knownBadFingerprints(cfg, logger)
	l1cfg := mediahealth.L1Config{
		Store:                  store,
		Renderer:               renderer,
		AsleepSince:            asleepSince,
		Admit:                  admit,
		Logger:                 logger,
		MaxPerNight:            cfg.L1.MaxPerNight,
		MinAsleep:              time.Duration(cfg.L1.MinAsleepSeconds) * time.Second,
		BlankVarianceThreshold: cfg.L1.BlankVarianceThreshold,
		KnownBad:               known,
		Settle:                 time.Duration(cfg.L1.SettleMs) * time.Millisecond,
		ConfirmSettle:          time.Duration(cfg.L1.ConfirmSettleMs) * time.Millisecond,
	}
	l1 := mediahealth.NewL1(l1cfg)
	logger.Info("media health: L1 render probe on",
		zap.Int("max_per_night", cfg.L1.MaxPerNight),
		zap.Int("min_asleep_seconds", cfg.L1.MinAsleepSeconds),
		zap.Int("known_bad_fingerprints", len(known)),
		zap.Bool("admission_gate", admit != nil))
	return l1
}

// knownBadFingerprints parses the l1 block's fingerprints, dropping (and
// logging) any that do not parse. Shared by the nightly render and the wall
// capture so both classify alike.
func knownBadFingerprints(cfg config.MediaHealthConfig, logger *zap.Logger) []mediahealth.Fingerprint {
	if cfg.L1 == nil {
		return nil
	}
	var known []mediahealth.Fingerprint
	for _, fp := range cfg.L1.KnownBad {
		parsed, err := mediahealth.ParseFingerprint(fp.PHash, fp.MaxDistance, fp.Label)
		if err != nil {
			logger.Warn("media health: dropping unparsable known-bad fingerprint", zap.Error(err))
			continue
		}
		known = append(known, parsed)
	}
	return known
}

// defaultWallMaxEdge is the wall capture's longer edge: the nightly
// render's viewport, so the two classify comparable frames.
const defaultWallMaxEdge = 1024

// wireWallCapture builds the wall-capture runner when
// mediaHealth.wallCapture.enabled is set; nil otherwise, with one log line.
func wireWallCapture(cfg config.MediaHealthConfig, svc *mediahealth.Service, kiosk screenshot.ArtworkCapturer, asleep func() bool, logger *zap.Logger) *mediahealth.WallCapture {
	if cfg.WallCapture == nil || !cfg.WallCapture.Enabled {
		logger.Info("media health: wall capture off (mediaHealth.wallCapture.enabled is not set)")
		return nil
	}
	if svc == nil || kiosk == nil {
		return nil
	}
	maxEdge := cfg.WallCapture.MaxEdge
	if maxEdge <= 0 || maxEdge > screenshot.MaxDimension {
		maxEdge = defaultWallMaxEdge
	}
	var threshold float64
	if cfg.L1 != nil {
		threshold = cfg.L1.BlankVarianceThreshold
	}
	known := knownBadFingerprints(cfg, logger)
	w := mediahealth.NewWallCapture(mediahealth.WallCaptureConfig{
		Store:                  svc.Store(),
		Capturer:               kioskWall{kiosk: kiosk, maxEdge: maxEdge},
		Deliver:                svc.ObserveWallFrame,
		Awake:                  func() bool { return asleep == nil || !asleep() },
		Settle:                 time.Duration(cfg.WallCapture.SettleMs) * time.Millisecond,
		KnownBad:               known,
		BlankVarianceThreshold: threshold,
		Logger:                 logger,
	})
	logger.Info("media health: wall capture on",
		zap.Int("settle_ms", cfg.WallCapture.SettleMs), zap.Int("max_edge", maxEdge),
		zap.Int("known_bad_fingerprints", len(known)))
	return w
}

// kioskWall adapts the screenshot package's artwork capture to the media
// health runner. A busy capturer (the hub's screenshot is running), a
// covered artwork and an unreachable kiosk are not evidence.
type kioskWall struct {
	kiosk   screenshot.ArtworkCapturer
	maxEdge int
}

func (k kioskWall) CaptureWall(ctx context.Context) (*mediahealth.WallFrame, error) {
	frame, err := k.kiosk.CaptureArtwork(ctx, k.maxEdge)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", mediahealth.ErrFrameUnavailable, err)
	}
	return &mediahealth.WallFrame{Image: frame.Image, Masks: frame.Masks, Clipped: frame.Clipped}, nil
}

// cdpSend is the kiosk CDP client's Send, the one method the hint pusher's
// sender needs.
type cdpSend func(method string, params map[string]interface{}) (interface{}, error)

// newHintSender builds the hint pusher's delivery: `setMediaHealth` over
// the kiosk CDP client through `window.handleCDPRequest`, the same
// evaluate the content-policy commands use, fenced on the player-session
// generation the same way — a reply that crossed a generation change came
// from a page that is gone, and the new page's boot cast carries its own
// hints. The map holds source URLs: it travels over loopback CDP only, as
// every displayPlaylist push already does.
func newHintSender(send cdpSend, generation func() uint64) mediahealth.HintSender {
	return func(_ context.Context, castSeq int64, hints map[string]mediahealth.Hint) ([]int, error) {
		cmd := commands.Command{Type: commands.CMD_SET_MEDIA_HEALTH, Arguments: map[string]any{
			"castSeq": castSeq,
			// Replace semantics: the full map for the wall, {} when nothing
			// is broken any more (a heal must reach the player too).
			"mediaHealth": hintMapArgument(hints),
		}}
		b, err := cmd.JSON()
		if err != nil {
			return nil, err
		}
		before := generation()
		result, err := send(cdp.METHOD_EVALUATE, map[string]interface{}{
			"expression":    fmt.Sprintf("window.handleCDPRequest(%s)", b),
			"awaitPromise":  true,
			"returnByValue": true,
		})
		if err != nil {
			return nil, err
		}
		if generation() != before {
			return nil, errors.New("media health: hint push raced a player generation change")
		}
		return decodeHintPushReply(result)
	}
}

func hintMapArgument(hints map[string]mediahealth.Hint) map[string]mediahealth.Hint {
	if hints == nil {
		return map[string]mediahealth.Hint{}
	}
	return hints
}

// decodeHintPushReply reads the player's `setMediaHealth` reply, bare or
// wrapped in the envelope's `message`: {ok:true, skippedIndices} is
// delivered; {ok:false, error:"staleCast"} is a cast race; any other
// answer is a refusal — a player whose manifest lists the command but
// does not implement it answers unknown-command, and must not be retried
// into for the rest of the cast.
func decodeHintPushReply(result interface{}) ([]int, error) {
	m, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%w: unexpected reply %T", mediahealth.ErrHintPushRefused, result)
	}
	if msg, ok := m["message"].(map[string]interface{}); ok {
		m = msg
	}
	if okValue, _ := m["ok"].(bool); !okValue {
		if code, _ := m["error"].(string); code == "staleCast" {
			return nil, mediahealth.ErrStaleCast
		}
		return nil, fmt.Errorf("%w: %v", mediahealth.ErrHintPushRefused, m["error"])
	}
	raw, _ := m["skippedIndices"].([]interface{})
	skipped := make([]int, 0, len(raw))
	for _, v := range raw {
		if f, ok := v.(float64); ok && f >= 0 {
			skipped = append(skipped, int(f))
		}
	}
	return skipped, nil
}
