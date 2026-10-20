package main

import (
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
// daemon run (docs/ff1-control-v1.md). With control.enabled=false (the
// default) newControlWiring returns nil and every transport is wired exactly
// as before: no authority, no wrappers, no policy.
type controlWiring struct {
	auth *control.Authority
}

// newControlWiring builds the authority when enabled. A failure to build it
// (no hostname) disables the protocol for this run rather than the daemon:
// controld is the recovery channel and must start.
func newControlWiring(cfg config.ControlConfig, deviceID string, logger *zap.Logger) *controlWiring {
	if !cfg.Enabled {
		return nil
	}
	auth, err := control.New(control.Config{DeviceID: deviceID, Enforced: cfg.Enforced})
	if err != nil {
		logger.Error("Control protocol enabled but could not start; running without it", zap.Error(err))
		return nil
	}
	// The boot line the fleet count reads (plan §3.1): one per start.
	st := auth.Status()
	fields := []zap.Field{zap.String("control_state", string(st.State)), zap.Bool("control_enforced", cfg.Enforced)}
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
func (c *controlWiring) wire(handler commandrouter.Handler, r relayer.Relayer, w ws.WS,
	h hub.Hub, m mediator.Mediator, executor devicectl.Executor, logger *zap.Logger) {
	if c == nil {
		return
	}
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
	}
}
