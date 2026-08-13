package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/netlog"
	"github.com/feral-file/ffos-user/components/feral-controld/softap"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// buildNetlogRecorder assembles the WAN-outage flight recorder
// (docs/wan-outage-observability.md stages 1-2). Returns nil when disabled by
// config or when the ring cannot be opened — the recorder is diagnostics and
// must never take the daemon down or block its startup; every consumer of the
// returned value nil-guards.
//
// ex is the devicectl executor and ds the status.DeviceStatus collector; the
// stage-2 egress seams (lastOutage on the collector, on-demand diagnostics
// and self-upload on the executor) are wired here through the same
// type-asserted-seam pattern as provisioning_wiring.go, so test doubles
// without the methods simply leave those paths unwired.
func buildNetlogRecorder(
	lifetimeCtx context.Context,
	conf *config.NetlogConfig,
	linkChecker *status.LinkChecker,
	exec wrapper.Exec,
	clock wrapper.Clock,
	dc dbus.DBus,
	relayerEndpoint string,
	ex, ds any,
	logger *zap.Logger,
) *netlog.Recorder {
	if conf != nil && conf.Disabled {
		logger.Info("netlog: flight recorder disabled by config")
		return nil
	}
	dir := netlog.DefaultDir
	var maxBytes int64
	if conf != nil {
		if conf.Dir != "" {
			dir = conf.Dir
		}
		maxBytes = conf.MaxTotalBytes
	}
	ring, err := netlog.OpenRing(dir, maxBytes)
	if err != nil {
		// Degrade, don't die: a read-only or full /home must not crash-loop
		// the recovery daemon for the sake of its own diagnostics.
		logger.Warn("netlog: ring unavailable, flight recorder disabled", zap.Error(err))
		return nil
	}
	// Tell the log uploader where the ring actually lives: a netlog.dir
	// override placing it outside ~/.logs must still ride every uploadLogs
	// bundle (the ring's primary egress), which otherwise only walks the
	// default location.
	if sink, ok := ex.(interface{ SetNetlogRingDir(string) }); ok {
		sink.SetNetlogRingDir(ring.Dir())
	}

	// The daemon-lifetime ctx: shared ladder passes must not run on a
	// caller's cancellable ctx (see netlog.Ladder.lifetimeCtx).
	ladder := netlog.NewLadder(
		lifetimeCtx,
		netlog.NewProber(linkChecker, exec, softap.ProfileName),
		netlogBackendHost(relayerEndpoint, logger),
		clock, logger)

	// Stage-2a self-upload arms only when the operator provisioned the
	// support-logs API key: the device ships with no credential for that API
	// (the uploadLogs COMMAND receives its key from the controller per call),
	// and probing the endpoint with a guessed key would only make noise.
	var selfUpload func()
	if conf != nil && strings.TrimSpace(conf.SelfUploadAPIKey) != "" {
		key := strings.TrimSpace(conf.SelfUploadAPIKey)
		if up, ok := ex.(interface{ SelfUploadLogs(string) }); ok {
			selfUpload = func() { up.SelfUploadLogs(key) }
		}
	} else {
		logger.Info("netlog: self-upload disabled (no netlog.selfUploadApiKey provisioned); ring still rides uploadLogs bundles")
	}

	rec := netlog.NewRecorder(netlog.RecorderOptions{
		Ring:          ring,
		Ladder:        ladder,
		Clock:         clock,
		Logger:        logger,
		SelfUpload:    selfUpload,
		InternetLevel: netlogInternetLevel(dc),
	})
	wireNetlogStatusSeams(ex, ds, rec)
	logger.Info("netlog: flight recorder ready", zap.String("dir", ring.Dir()))
	return rec
}

// netlogBackendHost extracts the relayer endpoint's hostname — the backend
// whose reachability the ladder's TCP/DNS rungs measure. A malformed endpoint
// falls back to the support-logs host (also a Feral File backend) so the
// ladder never dials an empty address.
func netlogBackendHost(relayerEndpoint string, logger *zap.Logger) string {
	if u, err := url.Parse(relayerEndpoint); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	logger.Warn("netlog: relayer endpoint unparsable, using support-logs host for backend probes",
		zap.String("endpoint", relayerEndpoint))
	return "support-logs.feralfile.com"
}

// errUnexpectedConnectivityReply marks a malformed GetConnectivityStatus
// reply — surfaced (not degraded to offline) per the level probe's contract.
var errUnexpectedConnectivityReply = errors.New("unexpected GetConnectivityStatus reply shape")

// netlogInternetLevel is the recorder's level half of the edge+level
// discipline: sys-monitord's CACHED verdict (refresh=false — one local D-Bus
// round-trip, never a network probe; monitord refreshes its own cache).
// Errors surface so the recorder skips the tick instead of fabricating an
// offline verdict — same posture as wireInternetProbe, for the same reason.
func netlogInternetLevel(dc dbus.DBus) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		deadlineCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		resp, err := dc.Call(
			deadlineCtx,
			dbus.MONITORD_NAME,
			dbus.MONITORD_PATH,
			dbus.MONITORD_INTERFACE,
			dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS,
			false,
		)
		if err != nil {
			return false, err
		}
		if len(resp) != 1 {
			return false, errUnexpectedConnectivityReply
		}
		connected, ok := resp[0].(bool)
		if !ok {
			return false, errUnexpectedConnectivityReply
		}
		return connected, nil
	}
}

// wireNetlogStatusSeams attaches the stage-2 read paths: the additive
// lastOutage field on the STATUS COLLECTOR (both the pulled getDeviceStatus
// reply and the poller's pushed device_status feed flow through GetStatus —
// stage 2b's point is that the backend sees the diagnosis without polling)
// and the runNetworkDiagnostics command on the executor.
func wireNetlogStatusSeams(ex, ds any, rec *netlog.Recorder) {
	if sink, ok := ds.(interface {
		SetLastOutageSource(func() *status.LastOutage)
	}); ok {
		sink.SetLastOutageSource(func() *status.LastOutage {
			o := rec.LastOutage()
			if o == nil {
				return nil
			}
			return &status.LastOutage{
				Start:    o.Start,
				End:      o.End,
				Class:    string(o.Class),
				Count24h: o.Count24h,
			}
		})
	}
	if sink, ok := ex.(interface {
		SetNetworkDiagnostics(func(context.Context) (any, error))
	}); ok {
		sink.SetNetworkDiagnostics(func(ctx context.Context) (any, error) {
			return rec.RunDiagnostics(ctx)
		})
	}
}

// netlogTransitionObserver adapts the recorder for provisioning.Config —
// nil recorder yields nil observer (the machine treats it as unwired).
func netlogTransitionObserver(rec *netlog.Recorder) func(string, string, string, string) {
	if rec == nil {
		return nil
	}
	return rec.ObserveProvTransition
}
