package netmetrics

import (
	"context"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	// pollInterval paces the telemetry reads. Two cheap nmcli list reads per
	// tick (no rescan, no dial) at half the vmagent 60s scrape cadence: every
	// scrape sees a fresh-enough value without the poller becoming load on an
	// already-sick network (the plan's probe-discipline constraint).
	pollInterval = 30 * time.Second

	// wifiReadTimeout bounds the nmcli wifi list read, same rationale as
	// linkcheck's probe timeout: a hung NetworkManager must not wedge the
	// poller goroutine.
	wifiReadTimeout = 5 * time.Second
)

// LinkProber is the consumer-owned seam onto status.LinkChecker.LinkTelemetry.
type LinkProber interface {
	LinkTelemetry(ctx context.Context) (wired, wifi bool, err error)
}

// Poller owns the one repeating telemetry read that feeds the probe-backed
// gauges (link state, Wi-Fi station). It is a pure producer: it never makes —
// and must never grow — recovery decisions, and the relayer gauges are fed
// event-driven by the relayer itself, not from here.
type Poller struct {
	link   LinkProber
	exec   wrapper.Exec
	clock  wrapper.Clock
	logger *zap.Logger
	done   chan struct{}
}

// NewPoller wires the telemetry poller. Start owns the goroutine; Stop (or ctx
// cancellation) ends it.
func NewPoller(link LinkProber, exec wrapper.Exec, clock wrapper.Clock, logger *zap.Logger) *Poller {
	return &Poller{
		link:   link,
		exec:   exec,
		clock:  clock,
		logger: logger,
		done:   make(chan struct{}),
	}
}

// Start launches the poll loop: one immediate tick (so the first scrape after
// startup already has link data), then every pollInterval.
func (p *Poller) Start(ctx context.Context) {
	go func() {
		p.logger.Info("Network telemetry poller started",
			zap.Duration("interval", pollInterval))
		p.tick(ctx)
		ticker := p.clock.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.done:
				return
			case <-ticker.C():
				p.tick(ctx)
			}
		}
	}()
}

// Stop ends the poll loop. Idempotent.
func (p *Poller) Stop() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

// tick performs one telemetry read and publishes it. All failures degrade to
// the honest encodings (LinkUnknown / absent station series) and are logged at
// Debug: a flapping network would otherwise turn the poller itself into log —
// and Sentry — noise (classified outage data is data, not errors).
func (p *Poller) tick(ctx context.Context) {
	wired, wifi, err := p.link.LinkTelemetry(ctx)
	if err != nil {
		p.logger.Debug("Link telemetry probe failed, exporting unknown", zap.Error(err))
		SetLinkState(LinkUnknown, LinkUnknown)
	} else {
		SetLinkState(boolGauge(wired), boolGauge(wifi))
	}

	p.tickWifiStation(ctx)
}

// tickWifiStation reads the in-use BSS from NetworkManager's cached scan list
// and publishes signal/identity, or clears the series when there is no
// association (or no answer — absence encodes both, deliberately).
func (p *Poller) tickWifiStation(ctx context.Context) {
	readCtx, cancel := context.WithTimeout(ctx, wifiReadTimeout)
	defer cancel()

	// --rescan no reads NM's cached BSS list without triggering an active
	// scan: an active scan every 30s would disturb the station (and fails
	// with EOPNOTSUPP in AP mode on the FF1 radio anyway).
	cmd := p.exec.CommandContext(readCtx, "nmcli", "-t", "-f",
		"IN-USE,SIGNAL,CHAN,BSSID", "device", "wifi", "list", "--rescan", "no")
	output, err := cmd.Output()
	if err != nil {
		p.logger.Debug("Wi-Fi station read failed, clearing station series", zap.Error(err))
		ClearWifiStation()
		return
	}

	station, ok := parseInUseStation(string(output))
	if !ok {
		ClearWifiStation()
		return
	}
	SetWifiStation(float64(station.signal), station.bssid, station.channel)
}

// wifiStation is one parsed in-use row from `nmcli -t device wifi list`.
type wifiStation struct {
	signal  int
	channel string
	bssid   string
}

// parseInUseStation finds the IN-USE ('*') row in terse nmcli wifi-list output
// and returns its signal/channel/BSSID. Field order matches the -f list above.
// A malformed in-use row (unparsable signal, empty BSSID) reports not-found:
// exporting a fabricated station is worse than exporting nothing.
func parseInUseStation(output string) (wifiStation, bool) {
	for _, line := range strings.Split(output, "\n") {
		fields := splitTerseFields(strings.TrimSpace(line))
		if len(fields) < 4 || fields[0] != "*" {
			continue
		}
		signal, err := strconv.Atoi(fields[1])
		if err != nil || fields[3] == "" {
			return wifiStation{}, false
		}
		return wifiStation{signal: signal, channel: fields[2], bssid: fields[3]}, true
	}
	return wifiStation{}, false
}

// splitTerseFields splits one `nmcli -t` line on ':' while honoring nmcli's
// backslash escaping — BSSIDs contain colons, which terse mode emits as `\:`.
// A trailing lone backslash is preserved literally rather than dropped.
func splitTerseFields(line string) []string {
	var fields []string
	var cur strings.Builder
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ':':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if escaped {
		cur.WriteRune('\\')
	}
	fields = append(fields, cur.String())
	return fields
}

func boolGauge(b bool) float64 {
	if b {
		return LinkUp
	}
	return LinkDown
}
