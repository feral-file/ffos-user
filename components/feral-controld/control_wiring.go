package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

// controlWiring is the feral-file#3551 control protocol as built for one
// daemon run (docs/ff1-control-v1.md). nil means the protocol is off and
// every transport is wired exactly as before: no authority, no wrappers, no
// policy.
type controlWiring struct {
	auth *control.Authority
}

// unknownDeviceID stands in when the hostname cannot be read but control
// state forces the protocol on: every envelope then fails its dev check, and
// the plaintext policy still applies. Failing closed beats starting open.
const unknownDeviceID = "FF1-UNKNOWN"

// newControlWiring decides whether the protocol runs and builds it.
//
// The decision FAILS CLOSED (spec §8). controld.json is replaced on every
// full-image OTA from a release-level secret, so config alone must never be
// able to switch a device back to open:
//   - control state on disk (the directory, its staging dir, or the
//     retirement tombstone; anything Stat cannot rule out counts) forces the
//     protocol on, whatever control.enabled says or whether it parsed;
//   - control.enabled only decides whether a device WITHOUT control state
//     starts the protocol (creates its identity, offers invitations);
//   - control.enforced only ever tightens enrolled to enforced.
func newControlWiring(cfg config.ControlConfig, deviceID, dir string, fsys control.FS, logger *zap.Logger) *controlWiring {
	hasState := control.StateExists(fsys, dir)
	if !hasState && !cfg.Enabled {
		return nil
	}
	if hasState && !cfg.Enabled {
		logger.Warn("Control state exists on disk; running the control protocol although control.enabled is off",
			zap.Bool("config_malformed", cfg.Malformed))
	}
	if deviceID == "" {
		if !hasState {
			logger.Error("Control protocol enabled but the hostname is unreadable; not starting it")
			return nil
		}
		logger.Error("Hostname unreadable; control state forces the protocol on with an unknown device id (envelopes will fail)")
		deviceID = unknownDeviceID
	}
	auth, err := control.New(control.Config{Dir: dir, DeviceID: deviceID, Enforced: cfg.Enforced, FS: fsys})
	if err != nil {
		// Only a programming error (empty DeviceID) gets here.
		logger.Error("Control protocol could not start", zap.Error(err))
		return nil
	}
	// The boot line the fleet count reads (plan §3.1): one per start.
	st := auth.Status()
	fields := []zap.Field{zap.String("control_state", string(st.State)), zap.Bool("control_enforced", cfg.Enforced),
		zap.Bool("control_enabled_config", cfg.Enabled), zap.Bool("control_state_on_disk", hasState)}
	if st.Profile != nil {
		fields = append(fields, zap.String("control_gen", st.Profile.Gen))
	}
	if lockErr := auth.LockErr(); lockErr != nil {
		logger.Error("Control protocol state is locked; envelopes refused until factory reset",
			append(fields, zap.Error(lockErr))...)
	} else {
		logger.Info("Control protocol state", fields...)
	}
	status.SetControlStatusSource(func() any { return auth.Status() })
	return &controlWiring{auth: auth}
}

// notificationHandles returns the relayer and WebSocket handles notification
// producers should use: sealing wrappers when the protocol is on, the raw
// handles otherwise.
func (c *controlWiring) notificationHandles(r relayer.Relayer, w ws.WS, logger *zap.Logger) (relayer.Relayer, ws.WS) {
	if c == nil {
		return r, w
	}
	return controlgate.WrapRelayer(r, c.auth, logger), controlgate.WrapWS(w, c.auth, logger)
}

// wire plugs the gate into the hub, the mediator and the executor.
func (c *controlWiring) wire(ctx context.Context, handler commandrouter.Handler, r relayer.Relayer, w ws.WS,
	h hub.Hub, m mediator.Mediator, executor devicectl.Executor, logger *zap.Logger) {
	if c == nil {
		return
	}
	// Enforced boot: overwrite the plaintext status history the relayer
	// keeps per topic (§9, §13). Background, best-effort, raw relayer.
	go controlgate.PurgeStalePlaintext(ctx, c.auth, r, logger, 2*time.Second, 10*time.Minute)
	gate := controlgate.New(c.auth, handler, logger)
	gate.SetPublisher(controlgate.NewPublisher(r, w, logger))
	if sink, ok := h.(interface{ SetControl(hub.ControlGate) }); ok {
		sink.SetControl(gate)
	} else {
		logger.Error("Hub does not accept the control gate; LAN control disabled")
	}
	if sink, ok := m.(interface{ SetControl(mediator.ControlGate) }); ok {
		sink.SetControl(gate)
	} else {
		logger.Error("Mediator does not accept the control gate; relayer control disabled")
	}
	if ex, ok := executor.(interface {
		SetControl(devicectl.ControlClaim)
		ControlOwnerEnrolled()
	}); ok {
		ex.SetControl(controlgate.ClaimQR{Auth: c.auth})
		gate.SetOwnerEnrolledHook(ex.ControlOwnerEnrolled)
	} else {
		logger.Error("Executor does not accept the control claim; invitation QR and retire disabled")
	}
}
