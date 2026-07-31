package offlinecache

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// This file is the resource-aware admission gate in front of the capture
// worker. The device this daemon runs on keeps a kiosk Chromium resident at
// all times and already lives near its memory and thermal limits; spawning
// the second, headless capture Chromium (ClassSoftware jobs) while the
// system is hot or memory-starved risks OOM-killing the kiosk or pushing
// the package toward the firmware thermal shutdown (Tjmax 95°C). The gate
// defers STARTING queued jobs while the device is under pressure — it never
// aborts a capture already in flight (there is deliberately no per-job
// cancellation; see ErrItemBusy's doc), and a deferred job stays StateQueued
// on the wire: deferral is a scheduling delay, not an error and not a new
// externally visible state (the state list is a mobile-app contract).

// Default admission thresholds. All are config-overridable via
// offlineCache.resourceGate (see config.OfflineCacheResourceGateConfig);
// these constants are the zero-config behavior. The numbers are anchored to
// the two independent protection layers that already exist OUTSIDE this
// daemon, so the gate always acts strictly earlier than they do:
//   - feral-watchdog: CPU 93°C sustained -> player notification; RAM 95%
//     sustained -> kiosk restart, escalating to reboot.
//   - firmware: thermal shutdown at Tjmax 95°C.
const (
	// WatchdogCriticalCPUTempC / WatchdogCriticalMemoryPercent mirror
	// feral-watchdog's own action thresholds (cpu.go's
	// CPU_CRITICAL_TEMPERATURE, ram.go's RAM_CRITICAL_THRESHOLD). Every
	// threshold below is expressed as a DERIVED berth beneath them rather
	// than as an independent magic number, so the gate provably acts
	// earlier than the layer it is protecting against and the
	// relationship survives future tuning. Kept in sync by hand — the two
	// daemons are separate Go modules — so a change to the watchdog's
	// constants must be mirrored here.
	WatchdogCriticalCPUTempC      = 93.0
	WatchdogCriticalMemoryPercent = 95.0

	// Software (headless-Chromium) admission. The two signals are
	// deliberately weighted differently: CPU temperature is STRICT and
	// memory comparatively generous, because the capture Chromium renders
	// WebGL entirely on the CPU (SwiftShader — see downloader.go's flag
	// comment), so its dominant hazard is sustained CPU load heating the
	// package toward the watchdog's 93°C line and the firmware Tjmax 95°C
	// shutdown, and degrading live kiosk playback along the way.
	//
	// The thermal berth is sized to cover a capture's own contribution:
	// HeadlessLimits caps that capture to a CPU subset (AllowedCPUs) and
	// a cycle quota, so the heat it can add on top of whatever the kiosk
	// is already producing is bounded — 18°C is the budget for it, which
	// also keeps captures out of the thermal envelope a heavy kiosk
	// artwork needs for itself. Raising the quota/cpuset without
	// re-examining this berth is the amendment hazard to watch.
	softwareThermalHeadroomC          = 18.0
	DefaultSoftwareBlockCPUTempC      = WatchdogCriticalCPUTempC - softwareThermalHeadroomC // 75°C
	DefaultSoftwareBlockMemoryPercent = 80.0

	// DefaultMemorySafetyCeilingPercent is the line a software capture's
	// WORST CASE must stay under, 5 points below the watchdog's
	// kiosk-restart threshold. It is what couples this gate to
	// HeadlessLimits.MemoryMaxBytes: see AdmissionPolicy's
	// SoftwareReserveBytes and softwareMemoryBlockPercent for how the
	// effective software memory threshold is derived from the cgroup
	// ceiling the capture is actually allowed to reach, instead of the
	// two settings being independent numbers that only happen to be
	// compatible at one particular RAM size.
	memorySafetyMarginPercent         = 5.0
	DefaultMemorySafetyCeilingPercent = WatchdogCriticalMemoryPercent - memorySafetyMarginPercent // 90%

	// Media (direct HTTP GET) admission — permissive: a streamed download
	// adds negligible CPU and RAM (no browser process at all), so it
	// yields only when the device is already hot/full enough that adding
	// ANY work is unjustifiable. Its berths are correspondingly narrow.
	mediaThermalHeadroomC          = 8.0
	DefaultMediaBlockCPUTempC      = WatchdogCriticalCPUTempC - mediaThermalHeadroomC // 85°C
	mediaMemoryHeadroomPercent     = 5.0
	DefaultMediaBlockMemoryPercent = WatchdogCriticalMemoryPercent - mediaMemoryHeadroomPercent // 90%

	// DefaultMetricsStaleAfter is the fail-open horizon: monitord publishes
	// sysmetrics roughly every 2 seconds, so a sample older than this means
	// ~7 consecutive missed signals — monitord or the bus is down, which is
	// not evidence of pressure. See AdmissionGate.Admit for the fail-open
	// rationale.
	DefaultMetricsStaleAfter = 15 * time.Second

	// Hysteresis: once a bucket blocks, it resumes only when the offending
	// signals drop this far BELOW their block thresholds. Without the gap,
	// a device hovering exactly at a threshold would flap admit/deny at
	// monitord's ~2s publish cadence. Deliberately unexported constants,
	// not config knobs — fewer ways to misconfigure the gate into flapping.
	admissionResumeMemoryDeltaPercent = 5.0
	admissionResumeTempDeltaC         = 5.0

	// DefaultAdmissionMaxDefer bounds how long the head of the queue may
	// sit deferred before it is failed with a visible reason instead of
	// blocking the FIFO forever. Waiting forever would hide a wedged queue
	// behind an eternal "queued"; proceeding anyway would defeat the gate
	// exactly when it matters (a device hot for this long is genuinely
	// distressed). Failing is safe: the queue is memory-only, failed is the
	// established re-issuable terminal state, and the reason string tells
	// the client why. An hour, not less: the software temperature
	// threshold above is strict enough that a single hot-running WebGL
	// artwork can legitimately hold the gate closed for its whole display
	// slot, and the bound should let a queued download wait that slot out
	// rather than fail midway through it.
	DefaultAdmissionMaxDefer = 60 * time.Minute

	// defaultAdmissionRetryInterval is how often the worker re-evaluates a
	// deferred head — a slight backoff from monitord's ~2s publish cadence
	// so re-checks never outpace fresh data.
	defaultAdmissionRetryInterval = 3 * time.Second
)

// AdmissionOptions bundles NewService's admission wiring. The zero value
// means "no gate" — identical behavior to the service before admission
// existed — so existing direct constructions (tests, chiefly) opt out by
// passing AdmissionOptions{}.
type AdmissionOptions struct {
	// Controller is consulted before each head-of-queue pop; nil admits
	// everything unconditionally.
	Controller AdmissionController
	// Clock drives deferral timing and the retry ticker; nil falls back
	// to the real clock.
	Clock wrapper.Clock
	// MaxDefer <= 0 means DefaultAdmissionMaxDefer.
	MaxDefer time.Duration
	// RetryInterval <= 0 means defaultAdmissionRetryInterval. Injectable
	// so service tests can drive deferral loops without real seconds.
	RetryInterval time.Duration
}

// AdmissionDecision is Admit's verdict. Reason is human-readable — used in
// defer/admit transition logs and, when a deferral outlives the service's
// max-defer bound, surfaced to the client via Coverage.Reason.
type AdmissionDecision struct {
	Allowed bool
	Reason  string
}

// AdmissionController is the service-owned seam the capture worker consults
// before popping the head of the queue. Admit is called under service.mu
// (inside dequeueAdmitted, so the decision and the pop are one critical
// section — see that function's doc): implementations MUST be cheap and
// non-blocking, and any internal lock MUST be a leaf that never acquires
// service.mu, directly or indirectly, or the two locks deadlock.
type AdmissionController interface {
	Admit(class MediaClass) AdmissionDecision
}

// AdmissionPolicy carries the block thresholds. Zero values are NOT
// defaulted here — OptionsFromConfig owns defaulting (the package-wide
// convention); a zero threshold in a directly constructed policy simply
// never blocks on that signal, which is the safe direction for tests.
type AdmissionPolicy struct {
	SoftwareBlockMemoryPercent float64
	SoftwareBlockCPUTempC      float64
	MediaBlockMemoryPercent    float64
	MediaBlockCPUTempC         float64
	// MetricsStaleAfter bounds how old the last sysmetrics sample may be
	// before the gate stops trusting it and fails open.
	MetricsStaleAfter time.Duration

	// SoftwareReserveBytes is the memory a software capture may consume
	// in the WORST case — in practice HeadlessLimits.MemoryMaxBytes, the
	// cgroup ceiling the capture Chromium is actually allowed to reach
	// (bootstrap.go wires the two together). When >0, the software
	// memory check becomes derived rather than a bare percentage: see
	// softwareMemoryBlockPercent.
	//
	// This coupling is the point. A static "block above 80%" is only
	// safe at one RAM size: with a 2 GiB cap on a 16 GB device a capture
	// admitted at 80% peaks near 92%, under the watchdog's 95% line —
	// but the SAME numbers on an 8 GB device peak at 105%, i.e. an OOM
	// the gate was supposed to prevent. Deriving the threshold from the
	// cap keeps the two settings correct together on any device and
	// under any reconfiguration of either one.
	//
	// The "worst case fits under the ceiling" guarantee is CONDITIONAL on
	// the cgroup cap actually being applied. It is configured from
	// intent, and the downloader degrades to an uncapped spawn when
	// transient systemd scopes are unavailable (no session bus, no
	// systemd-run — it warns and continues rather than failing captures
	// outright; see ensureScopeSupport). On that path the capture is
	// unbounded and the projection no longer holds.
	//
	// That degradation is safe by construction rather than by luck:
	// softwareMemoryBlockPercent takes the MINIMUM of this derived value
	// and the static threshold, so the coupling can only ever tighten
	// admission, never loosen it. An uncapped capture therefore runs
	// under a threshold at least as strict as the one that governed
	// before the cap existed, with feral-watchdog as the same backstop it
	// always was. Do NOT "fix" this by falling back to the static
	// threshold when the cap is inactive — static is the LOOSER of the
	// two, so that would relax admission exactly when the process is
	// unbounded.
	SoftwareReserveBytes int64
	// MemorySafetyCeilingPercent is the projected-usage line
	// SoftwareReserveBytes is measured against. <=0 disables the derived
	// check (falling back to SoftwareBlockMemoryPercent alone).
	MemorySafetyCeilingPercent float64
}

// admissionSample is the minimal decode of monitord's sysmetrics JSON
// payload. Deliberately a small local struct rather than an import:
// feral-sys-monitord is a separate Go module, and every existing controld
// consumer of this payload treats it as opaque bytes. Field tags mirror
// feral-sys-monitord/metric/metric.go's SysMetrics — if that wire shape
// ever changes, this decode (and executor.getSysMetrics's map-based one)
// must follow.
type admissionSample struct {
	CPU struct {
		CurrentTemperature float64 `json:"current_temperature"`
	} `json:"cpu"`
	Memory struct {
		MaxCapacity  float64 `json:"max_capacity"`  // MB
		UsedCapacity float64 `json:"used_capacity"` // MB
	} `json:"memory"`
}

// admissionBucket names the two independent threshold/latch buckets. Every
// MediaClass maps onto exactly one of them in bucketForClass.
type admissionBucket int

const (
	bucketSoftware admissionBucket = iota
	bucketMedia
)

// AdmissionGate implements AdmissionController from monitord's sysmetrics
// feed. Observe runs on the mediator's per-signal goroutine, Admit under
// service.mu — mu below is the leaf lock that makes both safe (see
// AdmissionController's lock-ordering contract).
type AdmissionGate struct {
	policy AdmissionPolicy
	clock  wrapper.Clock
	json   wrapper.JSON
	logger *zap.Logger

	mu       sync.Mutex
	sample   admissionSample
	sampleAt time.Time // zero = no sample ever observed
	// latched holds each bucket's block state so admit/resume use different
	// thresholds (hysteresis) — see the admissionResume* constants.
	latched [2]bool
	// staleWarned makes the fail-open warning fire once per staleness
	// episode instead of on every Admit call while monitord is down.
	staleWarned bool
	// reserveWarned makes the "cap does not fit this device" warning fire
	// once per process instead of on every Admit — the condition is a
	// static misconfiguration, not a transient state.
	reserveWarned bool
}

// NewAdmissionGate constructs the gate. The policy is trusted as-is (see
// AdmissionPolicy's doc on defaulting ownership).
func NewAdmissionGate(policy AdmissionPolicy, clock wrapper.Clock, json wrapper.JSON, logger *zap.Logger) *AdmissionGate {
	return &AdmissionGate{
		policy: policy,
		clock:  clock,
		json:   json,
		logger: logger,
	}
}

// Observe ingests one raw sysmetrics payload. It only decodes and stores —
// it must never block or take long: it runs inline on the mediator's D-Bus
// signal-dispatch goroutine (see Runtime.SysMetricsSink). A payload that
// fails to decode is dropped and the last good sample retained: a single
// corrupt signal must not blind the gate when the previous sample is still
// within the staleness horizon.
func (g *AdmissionGate) Observe(raw []byte) {
	var s admissionSample
	if err := g.json.Unmarshal(raw, &s); err != nil {
		g.logger.Debug("offline cache admission: dropping undecodable sysmetrics payload", zap.Error(err))
		return
	}
	g.mu.Lock()
	g.sample = s
	g.sampleAt = g.clock.Now()
	g.staleWarned = false
	g.mu.Unlock()
}

// Admit decides whether a job of class may start now.
//
// Fail-open posture: with no sample, a stale sample, or a nonsensical
// sample (MaxCapacity <= 0), the gate ADMITS. Absence of metrics is not
// evidence of pressure; downloads are explicitly user-initiated; the
// watchdog and firmware layers independently protect the kiosk either way;
// and failing closed would silently wedge the (memory-only) queue for
// reasons unrelated to load — strictly less available than the pre-gate
// behavior this feature replaced. The same reasoning applies per-signal: a
// broken sensor reads 0, and 0 never exceeds a block threshold.
func (g *AdmissionGate) Admit(class MediaClass) AdmissionDecision {
	bucket, bucketName := bucketForClass(class)

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.sampleAt.IsZero() || g.clock.Now().Sub(g.sampleAt) > g.policy.MetricsStaleAfter {
		if !g.staleWarned {
			g.staleWarned = true
			g.logger.Warn("offline cache admission: no fresh sysmetrics sample, failing open (admitting) until metrics resume",
				zap.Duration("stale_after", g.policy.MetricsStaleAfter),
				zap.Bool("ever_observed", !g.sampleAt.IsZero()))
		}
		// A stale episode also clears the latches: whatever pressure was
		// last seen is no longer knowable, and keeping a block latched on
		// dead data would be fail-closed by another name.
		g.latched[bucketSoftware] = false
		g.latched[bucketMedia] = false
		return AdmissionDecision{Allowed: true, Reason: "no fresh system metrics; failing open"}
	}

	blockMem, blockTemp := g.thresholdsFor(bucket, g.sample)
	memPct, memOK := memoryPercent(g.sample)
	temp := g.sample.CPU.CurrentTemperature

	if g.latched[bucket] {
		// Latched: resume only when every gated signal is below its resume
		// threshold (block - delta). An unreadable memory signal counts as
		// recovered — fail-open per-signal, same as the un-latched path.
		memRecovered := !memOK || memPct < blockMem-admissionResumeMemoryDeltaPercent
		tempRecovered := temp < blockTemp-admissionResumeTempDeltaC
		if memRecovered && tempRecovered {
			g.latched[bucket] = false
			g.logger.Info("offline cache admission: pressure cleared, resuming admissions",
				zap.String("bucket", bucketName),
				zap.Float64("memory_percent", memPct),
				zap.Float64("cpu_temp_c", temp))
			return AdmissionDecision{Allowed: true, Reason: "pressure cleared"}
		}
		return AdmissionDecision{
			Allowed: false,
			Reason:  pressureReason(bucketName, memPct, memOK, temp, blockMem, blockTemp),
		}
	}

	overMem := memOK && blockMem > 0 && memPct > blockMem
	overTemp := blockTemp > 0 && temp > blockTemp
	if overMem || overTemp {
		g.latched[bucket] = true
		reason := pressureReason(bucketName, memPct, memOK, temp, blockMem, blockTemp)
		g.logger.Info("offline cache admission: deferring under system pressure",
			zap.String("bucket", bucketName),
			zap.Float64("memory_percent", memPct),
			zap.Float64("memory_block_percent", blockMem),
			zap.Float64("cpu_temp_c", temp),
			zap.Float64("cpu_temp_block_c", blockTemp))
		return AdmissionDecision{Allowed: false, Reason: reason}
	}
	return AdmissionDecision{Allowed: true, Reason: "within thresholds"}
}

// thresholdsFor returns the block thresholds for a bucket. The software
// memory threshold is derived from the capture's own worst case against
// the current sample (see softwareMemoryBlockPercent); every other
// threshold is static policy.
func (g *AdmissionGate) thresholdsFor(bucket admissionBucket, s admissionSample) (blockMem, blockTemp float64) {
	if bucket == bucketSoftware {
		return g.softwareMemoryBlockPercent(s), g.policy.SoftwareBlockCPUTempC
	}
	return g.policy.MediaBlockMemoryPercent, g.policy.MediaBlockCPUTempC
}

// softwareMemoryBlockPercent is the effective software memory threshold:
// the stricter of the configured static percentage and the level at which
// a capture growing to its full cgroup ceiling (SoftwareReserveBytes)
// would still land under MemorySafetyCeilingPercent.
//
//	effective = min(static, ceiling - reserveAsPercentOfTotal)
//
// Taking the MINIMUM is what makes the two settings compose safely in
// both directions: the derived term can only ever tighten the operator's
// configured threshold, never loosen it. With the cap disabled
// (reserve <= 0) or an unusable sample, the derived term drops out and
// the static threshold stands — the pre-coupling behavior.
func (g *AdmissionGate) softwareMemoryBlockPercent(s admissionSample) float64 {
	static := g.policy.SoftwareBlockMemoryPercent
	if g.policy.SoftwareReserveBytes <= 0 || g.policy.MemorySafetyCeilingPercent <= 0 || s.Memory.MaxCapacity <= 0 {
		return static
	}
	// MaxCapacity is MB (monitord's wire unit); reserve is bytes.
	reservePercent := float64(g.policy.SoftwareReserveBytes) / (1024 * 1024) / s.Memory.MaxCapacity * 100
	derived := g.policy.MemorySafetyCeilingPercent - reservePercent
	if derived <= 0 {
		// The cap alone does not fit under the ceiling (a tiny device, or
		// memoryMaxBytes configured larger than the machine). Deriving
		// here would produce a threshold nothing can ever satisfy —
		// every software download would sit deferred until maxDefer
		// failed it, with no diagnosable cause. Fall back to the static
		// threshold and say so once: the cgroup ceiling is still enforced
		// by the kernel, so the worst case remains an OOM kill of the
		// CAPTURE (a clean, retryable failed job) rather than of the
		// kiosk — which is exactly the backstop MemoryMax exists to be.
		if !g.reserveWarned {
			g.reserveWarned = true
			g.logger.Warn("offline cache admission: headless memory cap exceeds the safety ceiling for this device's RAM, using the static memory threshold",
				zap.Int64("reserve_bytes", g.policy.SoftwareReserveBytes),
				zap.Float64("total_memory_mb", s.Memory.MaxCapacity),
				zap.Float64("safety_ceiling_percent", g.policy.MemorySafetyCeilingPercent))
		}
		return static
	}
	return min(static, derived)
}

// bucketForClass maps a job's MediaClass onto a threshold bucket. Only
// ClassSoftware pays the headless-Chromium cost; everything else — media,
// unknown (downloaded via the same direct-GET path, see MediaClass's doc),
// and any future class — is a plain HTTP stream and gets the permissive
// bucket.
func bucketForClass(class MediaClass) (admissionBucket, string) {
	if class == ClassSoftware {
		return bucketSoftware, "software"
	}
	return bucketMedia, "media"
}

// memoryPercent derives used-memory percent from a sample, reporting
// ok=false when the sample cannot support the division (never observed or
// a zero/negative capacity from a failed monitord read).
func memoryPercent(s admissionSample) (pct float64, ok bool) {
	if s.Memory.MaxCapacity <= 0 {
		return 0, false
	}
	return s.Memory.UsedCapacity / s.Memory.MaxCapacity * 100, true
}

// pressureReason renders a human-readable deferral cause. It names every
// signal currently at or past its block threshold (post-latch it also keeps
// naming signals still above their RESUME point, which is what "still
// blocked" means during hysteresis).
func pressureReason(bucketName string, memPct float64, memOK bool, temp, blockMem, blockTemp float64) string {
	reason := fmt.Sprintf("system under pressure (%s thresholds:", bucketName)
	if memOK && blockMem > 0 && memPct > blockMem-admissionResumeMemoryDeltaPercent {
		reason += fmt.Sprintf(" memory %.1f%% vs limit %.1f%%", memPct, blockMem)
	}
	if blockTemp > 0 && temp > blockTemp-admissionResumeTempDeltaC {
		reason += fmt.Sprintf(" cpu %.1f°C vs limit %.1f°C", temp, blockTemp)
	}
	return reason + ")"
}
