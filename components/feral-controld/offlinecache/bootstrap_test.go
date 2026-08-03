package offlinecache_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestOptionsFromConfig_NilConfigUsesDefaults(t *testing.T) {
	opts := offlinecache.OptionsFromConfig(nil, "http://127.0.0.1:9222")

	assert.False(t, opts.Enabled)
	assert.Equal(t, offlinecache.DefaultRootDir, opts.RootDir)
	assert.Equal(t, offlinecache.DefaultHeadlessBinaryPath, opts.HeadlessBinaryPath)
	assert.Equal(t, offlinecache.DefaultHeadlessUserDataDir, opts.HeadlessUserDataDir)
	assert.Equal(t, offlinecache.DefaultHeadlessDebugPort, opts.HeadlessDebugPort)
	assert.Equal(t, offlinecache.DefaultHeadlessIdleTeardown, opts.HeadlessIdleTeardown)
	assert.Equal(t, offlinecache.DefaultStaticServerAddr, opts.StaticServerAddr)
	assert.Equal(t, offlinecache.MissPolicyFailClosed, opts.MissPolicy)
	assert.Equal(t, "http://127.0.0.1:9222", opts.KioskCDPEndpoint)
	assert.Equal(t, int64(offlinecache.DefaultMaxDiskBytes), opts.MaxDiskBytes)
	assert.Zero(t, opts.CaptureWindowMs)
}

// TestOptionsFromConfig_UnsetMaxDiskBytesFallsBackToDefaultBudget is the
// regression test for the disk-exhaustion hazard DefaultMaxDiskBytes's
// doc describes: enabling offlineCache without an explicit
// maxDiskBytes must still leave the cache bounded (not "unlimited"),
// since this feature caches potentially gigabyte-scale assets on a
// disk-constrained device.
func TestOptionsFromConfig_UnsetMaxDiskBytesFallsBackToDefaultBudget(t *testing.T) {
	opts := offlinecache.OptionsFromConfig(&config.OfflineCacheConfig{Enabled: true}, "http://127.0.0.1:9222")
	assert.Equal(t, int64(offlinecache.DefaultMaxDiskBytes), opts.MaxDiskBytes)
}

func TestOptionsFromConfig_EmptyConfigUsesDefaultsButRespectsEnabled(t *testing.T) {
	opts := offlinecache.OptionsFromConfig(&config.OfflineCacheConfig{Enabled: true}, "http://127.0.0.1:9222")

	assert.True(t, opts.Enabled)
	assert.Equal(t, offlinecache.DefaultRootDir, opts.RootDir)
	assert.Equal(t, offlinecache.MissPolicyFailClosed, opts.MissPolicy)
}

func TestOptionsFromConfig_OverridesEveryField(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled:                     true,
		RootDir:                     "/custom/root",
		MaxDiskBytes:                123456,
		CaptureWindowMs:             5000,
		HeadlessBinaryPath:          "/custom/chromium",
		HeadlessUserDataDir:         "/custom/profile",
		HeadlessDebugPort:           9333,
		HeadlessIdleTeardownSeconds: 45,
		StaticServerAddr:            "127.0.0.1:9999",
		MissPolicy:                  "pass_through",
	}

	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")

	assert.True(t, opts.Enabled)
	assert.Equal(t, "/custom/root", opts.RootDir)
	assert.EqualValues(t, 123456, opts.MaxDiskBytes)
	assert.Equal(t, 5000, opts.CaptureWindowMs)
	assert.Equal(t, "/custom/chromium", opts.HeadlessBinaryPath)
	assert.Equal(t, "/custom/profile", opts.HeadlessUserDataDir)
	assert.Equal(t, 9333, opts.HeadlessDebugPort)
	assert.Equal(t, 45*time.Second, opts.HeadlessIdleTeardown)
	assert.Equal(t, "127.0.0.1:9999", opts.StaticServerAddr)
	assert.Equal(t, offlinecache.MissPolicy("pass_through"), opts.MissPolicy)
}

// The resource gate must be effective with zero configuration: absent
// section (and even absent OfflineCacheConfig entirely) means enabled with
// the built-in default policy.
func TestOptionsFromConfig_ResourceGateDefaultsOn(t *testing.T) {
	for name, cfg := range map[string]*config.OfflineCacheConfig{
		"nil config":     nil,
		"absent section": {Enabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")

			assert.True(t, opts.ResourceGate.Enabled)
			assert.Equal(t, float64(offlinecache.DefaultSoftwareBlockMemoryPercent), opts.ResourceGate.Policy.SoftwareBlockMemoryPercent)
			assert.Equal(t, float64(offlinecache.DefaultSoftwareBlockCPUTempC), opts.ResourceGate.Policy.SoftwareBlockCPUTempC)
			assert.Equal(t, float64(offlinecache.DefaultMediaBlockMemoryPercent), opts.ResourceGate.Policy.MediaBlockMemoryPercent)
			assert.Equal(t, float64(offlinecache.DefaultMediaBlockCPUTempC), opts.ResourceGate.Policy.MediaBlockCPUTempC)
			assert.Equal(t, offlinecache.DefaultMetricsStaleAfter, opts.ResourceGate.Policy.MetricsStaleAfter)
		})
	}
}

// The headless-Chromium resource cap must also be effective with zero
// configuration (same protect-by-default posture as the resource gate).
func TestOptionsFromConfig_HeadlessLimitsDefaultOn(t *testing.T) {
	for name, cfg := range map[string]*config.OfflineCacheConfig{
		"nil config":     nil,
		"absent section": {Enabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")

			assert.True(t, opts.HeadlessLimits.Enabled)
			assert.Equal(t, int64(offlinecache.DefaultHeadlessMemoryMaxBytes), opts.HeadlessLimits.MemoryMaxBytes)
			// The pin and the quota are both derived from the machine's
			// CPU count, so assert the ALIGNMENT INVARIANT that must hold
			// on any host rather than host-specific literals: the quota
			// can never exceed what the pinned set can deliver, or it
			// silently stops being a limit.
			assert.Regexp(t, `^0-\d+$`, opts.HeadlessLimits.AllowedCPUs)
			pinned := countAllowedCPUsForTest(t, opts.HeadlessLimits.AllowedCPUs)
			assert.GreaterOrEqual(t, pinned, 2, "a single-CPU pin would starve capture page load")
			assert.LessOrEqual(t, opts.HeadlessLimits.CPUQuotaPercent, pinned*100,
				"cpu quota must stay reachable within the pinned cpu set")
			assert.Positive(t, opts.HeadlessLimits.CPUQuotaPercent)
		})
	}
}

func TestOptionsFromConfig_HeadlessLimitsDisabledAndOverrides(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled: true,
		HeadlessLimits: &config.OfflineCacheHeadlessLimitsConfig{
			CPUQuotaPercent: 150,
			AllowedCPUs:     "4-7",
			MemoryMaxBytes:  1 << 30,
		},
	}
	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.True(t, opts.HeadlessLimits.Enabled)
	assert.Equal(t, 150, opts.HeadlessLimits.CPUQuotaPercent)
	assert.Equal(t, "4-7", opts.HeadlessLimits.AllowedCPUs)
	assert.Equal(t, int64(1<<30), opts.HeadlessLimits.MemoryMaxBytes)

	cfg.HeadlessLimits = &config.OfflineCacheHeadlessLimitsConfig{Disabled: true}
	opts = offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.False(t, opts.HeadlessLimits.Enabled)
	// Unset fields under a partial section still keep their defaults.
	assert.Positive(t, opts.HeadlessLimits.CPUQuotaPercent)
	assert.Equal(t, int64(offlinecache.DefaultHeadlessMemoryMaxBytes), opts.HeadlessLimits.MemoryMaxBytes)
}

// countAllowedCPUsForTest parses a systemd AllowedCPUs list the same way
// the package does, so alignment assertions read the effective CPU count
// rather than a literal.
func countAllowedCPUsForTest(t *testing.T, spec string) int {
	t.Helper()
	total := 0
	for _, part := range strings.Split(spec, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		start, err := strconv.Atoi(lo)
		require.NoError(t, err)
		if !isRange {
			total++
			continue
		}
		end, err := strconv.Atoi(hi)
		require.NoError(t, err)
		total += end - start + 1
	}
	return total
}

// TestOptionsFromConfig_ClampsUnreachableCPUQuota pins the internal
// alignment rule: a configured quota above what allowedCpus can deliver
// is not a 3-CPU budget on a 2-CPU pin — the cpuset caps it, so the quota
// stops limiting anything. It is clamped to the reachable value and the
// correction is surfaced for the caller to log.
func TestOptionsFromConfig_ClampsUnreachableCPUQuota(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled: true,
		HeadlessLimits: &config.OfflineCacheHeadlessLimitsConfig{
			CPUQuotaPercent: 300,
			AllowedCPUs:     "0-1", // 2 CPUs => 200% ceiling
		},
	}
	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.Equal(t, 200, opts.HeadlessLimits.CPUQuotaPercent)
	assert.Contains(t, opts.HeadlessLimitsWarning, "clamped to 200%")

	// A reachable quota is left alone and reports no warning.
	cfg.HeadlessLimits.CPUQuotaPercent = 150
	opts = offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.Equal(t, 150, opts.HeadlessLimits.CPUQuotaPercent)
	assert.Empty(t, opts.HeadlessLimitsWarning)

	// Multi-range specs count every CPU in the set.
	cfg.HeadlessLimits.AllowedCPUs = "0-1,4-5"
	cfg.HeadlessLimits.CPUQuotaPercent = 500
	opts = offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.Equal(t, 400, opts.HeadlessLimits.CPUQuotaPercent)
}

// TestOptionsFromConfig_CouplesAdmissionReserveToHeadlessMemoryCap pins
// the cross-setting coupling: the gate's software memory headroom is the
// cap the capture actually runs under, not an independent number.
func TestOptionsFromConfig_CouplesAdmissionReserveToHeadlessMemoryCap(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled:        true,
		HeadlessLimits: &config.OfflineCacheHeadlessLimitsConfig{MemoryMaxBytes: 3 << 30},
	}
	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.Equal(t, int64(3<<30), opts.ResourceGate.Policy.SoftwareReserveBytes)
	assert.Equal(t, float64(offlinecache.DefaultMemorySafetyCeilingPercent), opts.ResourceGate.Policy.MemorySafetyCeilingPercent)

	// Cap disabled: no reserve to account for, so the derived term drops
	// out and the static threshold governs.
	cfg.HeadlessLimits = &config.OfflineCacheHeadlessLimitsConfig{Disabled: true}
	opts = offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.Zero(t, opts.ResourceGate.Policy.SoftwareReserveBytes)

	// The coupling must hold on EVERY exit path, including the nil-config
	// one (see finalizeOptions) — not only when a config section exists.
	nilOpts := offlinecache.OptionsFromConfig(nil, "http://127.0.0.1:9222")
	assert.Equal(t, nilOpts.HeadlessLimits.MemoryMaxBytes, nilOpts.ResourceGate.Policy.SoftwareReserveBytes)
	assert.Positive(t, nilOpts.ResourceGate.Policy.SoftwareReserveBytes)
}

func TestOptionsFromConfig_ResourceGateDisabled(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled:      true,
		ResourceGate: &config.OfflineCacheResourceGateConfig{Disabled: true},
	}
	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.False(t, opts.ResourceGate.Enabled)
}

func TestOptionsFromConfig_ResourceGateOverrides(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled: true,
		ResourceGate: &config.OfflineCacheResourceGateConfig{
			SoftwareMaxMemoryPercent: 70,
			SoftwareMaxCPUTempC:      75,
			MediaMaxMemoryPercent:    85,
			MediaMaxCPUTempC:         88,
			MetricsStaleAfterSeconds: 30,
			MaxDeferSeconds:          600,
		},
	}
	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")

	assert.True(t, opts.ResourceGate.Enabled)
	assert.Equal(t, 70.0, opts.ResourceGate.Policy.SoftwareBlockMemoryPercent)
	assert.Equal(t, 75.0, opts.ResourceGate.Policy.SoftwareBlockCPUTempC)
	assert.Equal(t, 85.0, opts.ResourceGate.Policy.MediaBlockMemoryPercent)
	assert.Equal(t, 88.0, opts.ResourceGate.Policy.MediaBlockCPUTempC)
	assert.Equal(t, 30*time.Second, opts.ResourceGate.Policy.MetricsStaleAfter)
	// maxDeferSeconds is accepted but inert: deferral no longer has a
	// deadline. Setting it must say so rather than be quietly dropped.
	assert.Contains(t, opts.ResourceGateWarning, "maxDeferSeconds")
	assert.Contains(t, opts.ResourceGateWarning, "no longer honored")
}

// A partial resourceGate section overrides only what it sets; every other
// knob keeps its default (the <=0-means-default convention).
func TestOptionsFromConfig_ResourceGatePartialOverrideKeepsDefaults(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled: true,
		ResourceGate: &config.OfflineCacheResourceGateConfig{
			SoftwareMaxMemoryPercent: 70,
		},
	}
	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")

	assert.Equal(t, 70.0, opts.ResourceGate.Policy.SoftwareBlockMemoryPercent)
	assert.Equal(t, float64(offlinecache.DefaultSoftwareBlockCPUTempC), opts.ResourceGate.Policy.SoftwareBlockCPUTempC)
	assert.Equal(t, float64(offlinecache.DefaultMediaBlockMemoryPercent), opts.ResourceGate.Policy.MediaBlockMemoryPercent)
	// A resourceGate section that does not mention maxDeferSeconds warns
	// about nothing.
	assert.Empty(t, opts.ResourceGateWarning)
}

func TestBootstrap_WiresEveryComponent(t *testing.T) {
	dir := t.TempDir()
	opts := offlinecache.OptionsFromConfig(&config.OfflineCacheConfig{
		Enabled:          true,
		RootDir:          dir,
		StaticServerAddr: "127.0.0.1:0",
	}, "http://127.0.0.1:9222")

	rt := offlinecache.Bootstrap(
		opts, nil, nil,
		wrapper.NewHTTPClient(), wrapper.NewWebSocketDialer(nil),
		wrapper.NewOS(), wrapper.NewIO(), wrapper.NewJSON(), wrapper.NewExec(), wrapper.NewClock(),
		zaptest.NewLogger(t),
	)

	require.NotNil(t, rt.Service)
	require.NotNil(t, rt.KioskReplay)
	require.NotNil(t, rt.StaticServer)
	// The default-on resource gate exposes its metrics sink for main.go's
	// mediator wiring.
	require.NotNil(t, rt.SysMetricsSink)

	// Bootstrap must not have started anything (no I/O at construction
	// time) — Start-ing the service should still work against a freshly
	// wired store.
	require.NoError(t, rt.Service.Start(t.Context()))
	rt.Service.Stop()
}

// A disabled resource gate leaves no sink to wire — main.go's nil guard is
// the off switch.
func TestBootstrap_DisabledResourceGateHasNoSysMetricsSink(t *testing.T) {
	dir := t.TempDir()
	opts := offlinecache.OptionsFromConfig(&config.OfflineCacheConfig{
		Enabled:          true,
		RootDir:          dir,
		StaticServerAddr: "127.0.0.1:0",
		ResourceGate:     &config.OfflineCacheResourceGateConfig{Disabled: true},
	}, "http://127.0.0.1:9222")

	rt := offlinecache.Bootstrap(
		opts, nil, nil,
		wrapper.NewHTTPClient(), wrapper.NewWebSocketDialer(nil),
		wrapper.NewOS(), wrapper.NewIO(), wrapper.NewJSON(), wrapper.NewExec(), wrapper.NewClock(),
		zaptest.NewLogger(t),
	)

	assert.Nil(t, rt.SysMetricsSink)
}

// TestOptionsFromConfig_WarnsOnUnparseableAllowedCPUs pins the config-shape
// trap the CPU spec sits in: systemd's AllowedCPUs= accepts
// whitespace-separated lists that `taskset -c` rejects outright, so an
// operator can copy a perfectly legal systemd value into config and lose
// the CPU pin — the limit that bounds package power draw — without a word
// being said. A configured-but-inert pin must be as loud as any other kind
// of inertness here.
func TestOptionsFromConfig_WarnsOnUnparseableAllowedCPUs(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled: true,
		HeadlessLimits: &config.OfflineCacheHeadlessLimitsConfig{
			CPUQuotaPercent: 300,
			AllowedCPUs:     "0 1 2 3", // systemd accepts this; taskset does not
		},
	}
	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.Contains(t, opts.HeadlessLimitsWarning, "WITHOUT a cpu pin")
	assert.Contains(t, opts.HeadlessLimitsWarning, "0 1 2 3")
	// The spec is reported, not silently rewritten: the systemd property is
	// still worth passing (it becomes real if cpuset is ever delegated), and
	// the quota is untouched since there is no cpuset ceiling to clamp to.
	assert.Equal(t, "0 1 2 3", opts.HeadlessLimits.AllowedCPUs)
	assert.Equal(t, 300, opts.HeadlessLimits.CPUQuotaPercent)

	// The same spec written in a form taskset accepts warns about nothing.
	// (An empty AllowedCPUs is NOT the contrast case: at the config layer
	// it means "keep the derived default", not "no pin".)
	cfg.HeadlessLimits.AllowedCPUs = "0-3"
	opts = offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")
	assert.Empty(t, opts.HeadlessLimitsWarning)
	assert.Equal(t, "0-3", opts.HeadlessLimits.AllowedCPUs)
}
