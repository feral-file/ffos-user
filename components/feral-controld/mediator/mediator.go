package mediator

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mdns"
	playlist_refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

//go:generate mockgen -source=mediator.go -destination=../mocks/mediator.go -package=mocks -mock_names=Mediator=MockMediator

type Mediator interface {
	Start()
	Stop()
	// InitializeMDNS keys advertising on link state (status.LinkState), not
	// internet reachability, so the LAN recovery hub stays discoverable on any
	// LAN even with no upstream internet.
	InitializeMDNS(advertiser mdns.Advertiser, info mdns.DeviceInfo, link status.LinkState)
	SetClaimed(claimed bool)
	// SetTopicObserver registers a callback invoked when the relayer assigns
	// this device its system topic (empty -> non-empty transition). Wired once
	// at composition time, before Start; main uses it to re-trigger the
	// auto-claim flow, whose topic wait may already have expired by the time
	// the assignment arrives.
	SetTopicObserver(observer func())
	// SetSysMetricsObserver registers a sink for monitord's raw sysmetrics
	// payloads (forwarded verbatim, undecoded — decode and policy live in
	// the consumer, keeping this mediator pure forwarding glue). Wired once
	// at composition time, before Start; main uses it to feed the offline
	// cache's admission gate. The sink runs inline on the D-Bus per-signal
	// goroutine and must not block.
	SetSysMetricsObserver(observer func(raw []byte))
}

type mediator struct {
	relayer    relayer.Relayer
	dbus       dbus.DBus
	cdp        cdp.CDP
	cmdHandler commandrouter.Handler
	executor   devicectl.Executor
	logger     *zap.Logger
	refresher  playlist_refresher.Refresher
	json       wrapper.JSON

	// topicObserver, when set, fires on the empty->non-empty system-topic
	// assignment (see Mediator.SetTopicObserver). Set once before Start, read
	// on the relayer-message goroutine; no lock by the same single-writer
	// argument as the executor's claimObserver.
	topicObserver func()

	// sysMetricsObserver, when set, receives every raw sysmetrics payload
	// (see Mediator.SetSysMetricsObserver). Same set-once-before-Start,
	// single-writer discipline as topicObserver.
	sysMetricsObserver func(raw []byte)

	// relayerReconciling collapses overlapping heartbeat-driven relayer
	// reconciles to one in flight: signal handlers run on per-signal
	// goroutines and heartbeats outpace RetryableConnect's transient-retry
	// loop. Overlaps would be tolerated (ErrAlreadyConnected is success) but
	// are pointless work against the relayer endpoint.
	relayerReconciling atomic.Bool

	mdnsMu         sync.Mutex
	mdnsAdvertiser mdns.Advertiser
	mdnsDeviceInfo mdns.DeviceInfo
	linkState      status.LinkState
	// mdnsStarted mirrors whether the advertiser is currently registered, so the
	// periodic self-heal (see reconcileMDNSLocked) can tell "needs starting" from
	// "already up" without relying on Advertiser.Start's already-started error.
	mdnsStarted bool
}

// startMDNSLocked registers the advertiser with the current device info and
// records the started state. Caller holds mdnsMu. No-op guidance: callers gate
// on link presence; this only touches the started flag on a successful Start so
// a failed Start is retried by the next reconcile.
func (m *mediator) startMDNSLocked() {
	if m.mdnsAdvertiser == nil || m.mdnsStarted {
		return
	}
	if err := m.mdnsAdvertiser.Start(m.mdnsDeviceInfo); err != nil {
		m.logger.Warn("Failed to start mDNS advertiser", zap.Error(err))
		return
	}
	m.mdnsStarted = true
}

// stopMDNSLocked tears the advertiser down and clears the started state. Caller
// holds mdnsMu. Idempotent.
func (m *mediator) stopMDNSLocked() {
	if m.mdnsAdvertiser == nil || !m.mdnsStarted {
		return
	}
	m.mdnsAdvertiser.Stop()
	m.mdnsStarted = false
}

// reconcileMDNSLocked drives the advertiser to match link state: advertising
// while a local link exists, down otherwise. It is the self-heal for the gap
// that sys-monitord's connectivity_change signal cannot cover — that signal
// fires only on INTERNET-reachability transitions, so a link that comes up while
// the internet stays down (LAN switch with a dead WAN — precisely the case the
// recovery hub exists for) would otherwise never start the advertiser. Driven
// off the periodic SYSMETRICS signal, which arrives regardless of internet
// state, so discovery recovers within one metrics interval. Caller holds mdnsMu.
func (m *mediator) reconcileMDNSLocked(ctx context.Context) {
	if m.mdnsAdvertiser == nil {
		return
	}
	hasLink := m.linkState != nil && m.linkState.HasLink(ctx)
	switch {
	case hasLink && !m.mdnsStarted:
		m.startMDNSLocked()
	case !hasLink && m.mdnsStarted:
		m.stopMDNSLocked()
	}
}

func New(
	relayer relayer.Relayer,
	dbus dbus.DBus,
	cdp cdp.CDP,
	cmdHandler commandrouter.Handler,
	executor devicectl.Executor,
	refresher playlist_refresher.Refresher,
	json wrapper.JSON,
	l *zap.Logger,
) Mediator {
	return &mediator{
		relayer:    relayer,
		dbus:       dbus,
		cdp:        cdp,
		cmdHandler: cmdHandler,
		executor:   executor,
		json:       json,
		logger:     l,
		refresher:  refresher,
	}
}

func (m *mediator) Start() {
	m.dbus.OnBusSignal(m.handleDBusSignal)
	m.relayer.OnRelayerMessage(m.handleRelayerMessage)
}

func (m *mediator) Stop() {
	m.relayer.RemoveRelayerMessage(m.handleRelayerMessage)
	m.dbus.RemoveBusSignal(m.handleDBusSignal)
}

// InitializeMDNS registers the advertiser and starts it when a local link is
// present. Advertising is keyed on link state, not internet reachability: the
// LAN hub is the BLE-replacement recovery channel and must be discoverable on
// any LAN even with no upstream internet.
func (m *mediator) InitializeMDNS(advertiser mdns.Advertiser, info mdns.DeviceInfo, link status.LinkState) {
	m.mdnsMu.Lock()
	defer m.mdnsMu.Unlock()

	m.mdnsAdvertiser = advertiser
	m.mdnsDeviceInfo = info
	m.linkState = link

	if link != nil && link.HasLink(context.Background()) {
		m.startMDNSLocked()
	}
}

// SetTopicObserver registers the topic-assignment callback. See the Mediator
// interface doc.
func (m *mediator) SetTopicObserver(observer func()) {
	m.topicObserver = observer
}

// SetSysMetricsObserver registers the raw-sysmetrics sink. See the Mediator
// interface doc.
func (m *mediator) SetSysMetricsObserver(observer func(raw []byte)) {
	m.sysMetricsObserver = observer
}

// SetClaimed updates the advertised claim state. Because zeroconf only publishes
// its TXT record once at Register time, reflecting a claim-state change requires
// a Stop+Start re-registration with the updated TXT. This is a no-op when the
// state is unchanged so repeated claim signals do not churn the advertiser.
func (m *mediator) SetClaimed(claimed bool) {
	m.mdnsMu.Lock()
	defer m.mdnsMu.Unlock()

	if m.mdnsDeviceInfo.Claimed == claimed {
		return
	}
	m.mdnsDeviceInfo.Claimed = claimed

	if m.mdnsAdvertiser == nil {
		return
	}

	m.logger.Info("Re-registering mDNS after claim-state change", zap.Bool("claimed", claimed))
	m.stopMDNSLocked()
	// Only re-advertise while a link exists, mirroring the link-keyed lifecycle;
	// if the link is down the periodic reconcile (SYSMETRICS) brings it back with
	// the current (now updated) TXT once a link appears.
	if m.linkState != nil && m.linkState.HasLink(context.Background()) {
		m.startMDNSLocked()
	}
}

// reconcileRelayer drives the relayer connection to its desired state —
// connected whenever the device is online — off the periodic SYSMETRICS
// heartbeat. It is deliberately cheap on the steady path: a connected relayer
// short-circuits before any D-Bus traffic. Single-flight via
// relayerReconciling (see the field comment).
func (m *mediator) reconcileRelayer(ctx context.Context) {
	if m.relayer.IsConnected() {
		return
	}
	if !m.relayerReconciling.CompareAndSwap(false, true) {
		return
	}
	defer m.relayerReconciling.Store(false)

	connected, err := m.queryConnectivity(ctx)
	if err != nil {
		// Assumed-offline discipline: an unanswerable query reads as offline;
		// Debug because this repeats every heartbeat while degraded.
		m.logger.Debug("Relayer reconcile: connectivity unknown, assuming offline", zap.Error(err))
		return
	}
	if !connected {
		return
	}

	m.logger.Info("Relayer reconcile: device online with relayer disconnected, connecting")
	if err := m.relayer.RetryableConnect(ctx); err != nil {
		m.logger.Error("Relayer reconcile: connect failed, retrying on a later heartbeat", zap.Error(err))
	} else {
		m.logger.Info("Relayer reconcile: relayer connected")
	}
}

// queryConnectivity reads sys-monitord's CACHED internet reachability
// (refresh=false): this runs every metrics interval while the relayer is
// down, so it must cost one local D-Bus round-trip, never a network probe —
// the same polling discipline as the hub status internet probe. sys-monitord
// refreshes its own cache, so a stale answer heals within its refresh period.
func (m *mediator) queryConnectivity(ctx context.Context) (bool, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	resp, err := m.dbus.Call(
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
		return false, fmt.Errorf("expected 1 response, got %d", len(resp))
	}
	connected, ok := resp[0].(bool)
	if !ok {
		return false, fmt.Errorf("expected bool, got %T", resp[0])
	}
	return connected, nil
}

func (m *mediator) handleDBusSignal(
	ctx context.Context,
	payload godbus.DBusPayload) ([]interface{}, error) {
	if payload.Member.IsACK() {
		return nil, nil
	}

	if payload.Member != dbus.MONITORD_EVENT_SYSMETRICS {
		m.logger.Info("handle received DBus signal", zap.String("name", payload.Name()), zap.String("path", payload.Path.String()))
	}

	switch payload.Member {
	case dbus.MONITORD_EVENT_SYSMETRICS:
		if len(payload.Body) != 1 {
			m.logger.Error("Invalid number of arguments", zap.Int("expected", 1), zap.Int("actual", len(payload.Body)))
			return nil, fmt.Errorf("invalid number of arguments")
		}

		body, ok := payload.Body[0].([]byte)
		if !ok {
			m.logger.Error("Invalid body type", zap.String("expected", "[]byte"), zap.String("actual", reflect.TypeOf(payload.Body[0]).String()))
			return nil, fmt.Errorf("invalid body type")
		}

		m.logger.Debug("Received sysmetrics", zap.String("metrics", string(body)))
		m.executor.SaveLastSysMetrics(body)
		if m.sysMetricsObserver != nil {
			// Raw forward only — decode and policy live in the consumer
			// (the offline cache's admission gate today).
			m.sysMetricsObserver(body)
		}

		// Self-heal mDNS discoverability off this periodic signal. connectivity_change
		// only fires on INTERNET transitions, so a link that comes up while the
		// internet stays down (LAN with a dead WAN — the recovery hub's raison
		// d'être) never triggers the advertiser there. SYSMETRICS arrives regardless
		// of internet state, so reconciling here recovers discovery within one
		// metrics interval. Idempotent: a no-op when the advertiser already matches
		// link state.
		m.mdnsMu.Lock()
		m.reconcileMDNSLocked(ctx)
		m.mdnsMu.Unlock()

		// The relayer connection gets the same level-triggered treatment off
		// this heartbeat as mDNS above. This is the house discipline, not a
		// coincidence: connectivity_change fires only on internet-reachability
		// TRANSITIONS, so any consumer that missed its edge stays wrong until
		// the state happens to transition again. The startup relayer gate is
		// exactly such a consumer — it evaluates connectivity once, and when
		// that snapshot was taken while D-Bus was still down (non-fatal start
		// with background retry) on a network that is already online, no
		// transition ever comes: the relayer never connects, no topic is
		// assigned, and auto-claim strands even though provisioning sees the
		// device online. Reconciling here converges within one metrics
		// interval instead. When the heartbeat itself is absent (D-Bus or
		// sys-monitord down), connectivity is unknowable and the
		// assumed-offline discipline applies — there is nothing to reconcile
		// toward.
		m.reconcileRelayer(ctx)

	case dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE:
		if len(payload.Body) != 1 {
			m.logger.Error("Invalid number of arguments", zap.Int("expected", 1), zap.Int("actual", len(payload.Body)))
			return nil, fmt.Errorf("invalid number of arguments")
		}

		connected, ok := payload.Body[0].(bool)
		if !ok {
			m.logger.Error("Invalid body type", zap.String("expected", "bool"), zap.String("actual", reflect.TypeOf(payload.Body[0]).String()))
			return nil, fmt.Errorf("invalid body type")
		}

		m.logger.Info("Received connectivity change event",
			zap.Bool("connected", connected),
			zap.Bool("relayer_connected", m.relayer.IsConnected()),
			zap.Bool("mdns_active", m.mdnsAdvertiser != nil),
		)

		// Send the connectivity change to web app
		m.logger.Debug("Forwarding connectivity change to web app", zap.Bool("connected", connected))
		_, err := m.cdp.Send(
			cdp.METHOD_EVALUATE,
			map[string]interface{}{
				"expression": fmt.Sprintf("window.handleConnectivityChange(%t)", connected),
			})
		if err != nil {
			// handleConnectivityChange is a notify-only hook; player builds that
			// return nothing surface as "type mismatch: undefined" from the CDP
			// client — the call was delivered, there is just no return value.
			// Only real delivery failures deserve a visible log.
			if strings.Contains(err.Error(), "type mismatch: undefined") {
				m.logger.Debug("Connectivity change delivered; player handler returned no value")
			} else {
				m.logger.Warn("Failed to forward connectivity change to web app", zap.Error(err))
			}
		}

		// Reconnect the relayer if it's not already connected
		if connected && !m.relayer.IsConnected() {
			m.logger.Info("Connectivity restored, reconnecting relayer")
			err := m.relayer.RetryableConnect(ctx)
			if err != nil {
				m.logger.Error("Failed to reconnect to relayer", zap.Error(err))
			} else {
				m.logger.Info("Relayer reconnected after connectivity change")
			}
		}

		// Re-register mDNS on connectivity changes. The Stop+Start is preserved
		// from the original interface-change handling: a connectivity event can
		// accompany an interface set change, and the advertiser's sockets bind
		// specific interfaces, so stale sockets must be torn down and fresh ones
		// re-registered. What changed is the *gate*: whether to re-advertise is
		// now keyed on LINK state, not the internet-reachability `connected`
		// flag. Losing internet while a LAN link remains must NOT take the LAN
		// recovery hub's discoverability down — that was exactly the old bug.
		//
		// A link that comes up while the internet stays down does NOT fire this
		// handler; that gap is covered by the periodic SYSMETRICS reconcile above.
		// Here we force a Stop+Start rebind (not a plain reconcile) because a
		// connectivity event can accompany an interface-set change and the
		// advertiser's sockets bind specific interfaces, so stale sockets must be
		// torn down even when the advertiser was already up.
		m.mdnsMu.Lock()
		if m.mdnsAdvertiser != nil {
			// Unconditional Stop (not the guarded stopMDNSLocked): an interface-set
			// change can accompany this event, so any lingering zeroconf server bound
			// to a stale interface must be torn down before re-registering, even if we
			// believe we were not advertising. Stop is idempotent when not registered.
			m.mdnsAdvertiser.Stop()
			m.mdnsStarted = false
			if m.linkState != nil && m.linkState.HasLink(ctx) {
				m.startMDNSLocked()
			}
		}
		m.mdnsMu.Unlock()

	default:
		m.logger.Warn("Unknown signal", zap.String("member", payload.Member.String()))
	}

	return nil, nil
}

func (m *mediator) handleRelayerMessage(ctx context.Context, payload relayer.Payload) error {
	payloadJSON, _ := m.json.Marshal(payload)
	logPayload := helper.TruncateBytes(payloadJSON, logger.MAX_FIELD_LENGTH)
	m.logger.Info("handle received relayer message",
		zap.ByteString("payload", logPayload),
		zap.String("messageID", payload.MessageID),
		zap.String("command", func() string {
			if payload.Message.Command == nil {
				return ""
			}
			return *payload.Message.Command
		}()),
	)

	switch payload.MessageID {
	case relayer.MESSAGE_ID_SYSTEM:
		topicID := payload.Message.TopicID
		if topicID == nil {
			err := fmt.Errorf("payload doesn't contain topicID")
			m.logger.Error("Payload doesn't contain topicID", zap.ByteString("payload", logPayload))
			return err
		}

		// Save state
		s := state.GetState()
		hadTopic := strings.TrimSpace(s.Relayer.TopicID) != ""
		s.Relayer.TopicID = *topicID
		err := s.Save()
		if err != nil {
			m.logger.Error("Failed to persist state", zap.Error(err))
			return err
		}
		// First assignment (empty -> set) is the factory-fresh moment the
		// auto-claim flow may have given up waiting for (its topic wait is
		// bounded and only an online TRANSITION re-triggers it, which a device
		// that stays online never produces). Notify AFTER the persist so the
		// observer's own state reads see the topic. Edge-gated: a topic
		// rotation on an already-claimed device must not re-fire.
		if !hadTopic && strings.TrimSpace(*topicID) != "" && m.topicObserver != nil {
			m.logger.Info("Relayer system topic assigned; notifying topic observer")
			m.topicObserver()
		}

	default:
		var commandType commands.Type
		if payload.Message.Command != nil {
			commandType = commands.Type(*payload.Message.Command)
		}
		command := commands.Command{
			Type:      commandType,
			Arguments: payload.Message.Request,
		}
		result, err := m.cmdHandler.Process(ctx, command)
		if err != nil {
			if commandrouter.IsRateLimited(err) {
				// Report a legible rejection back to the caller rather than
				// dropping the command silently (feral-file/ffos-user#208).
				m.logger.Warn("Command rejected by storm protection",
					zap.String("command", commandType.String()),
					zap.Error(err),
				)
				resp := relayer.Response{
					Type:      "RPC",
					MessageID: payload.MessageID,
					Message: map[string]any{
						"error":   "rate_limited",
						"command": commandType.String(),
						"message": err.Error(),
					},
				}
				return m.relayer.Send(ctx, resp)
			}
			m.logger.Error("Failed to process command", zap.Error(err))
			return err
		}
		if result == nil {
			m.logger.Warn("Processed command returned no result", zap.ByteString("payload", logPayload))
			return nil
		}

		resp := relayer.Response{
			Type:      "RPC",
			MessageID: payload.MessageID,
			Message:   result,
		}

		m.logger.Debug("Sending relayer RPC response", zap.String("messageID", payload.MessageID))
		return m.relayer.Send(ctx, resp)
	}

	return nil
}
