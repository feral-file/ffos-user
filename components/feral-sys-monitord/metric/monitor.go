package metric

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/wrapper"
)

// SystemType represents the type of system
type SystemType int

const (
	SystemTypeIntel SystemType = iota
	SystemTypeAMD
)

// Prometheus metrics
var (
	CPUTemperatureCelsius = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cpu_temperature_celsius",
		Help: "Current CPU temperature in Celsius",
	})
	CPUUptimeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cpu_uptime_seconds",
		Help: "Current CPU uptime in seconds",
	})
)

type CPUMetrics struct {
	MaxFrequency       float64 `json:"max_frequency"`
	CurrentFrequency   float64 `json:"current_frequency"`
	MaxTemperature     float64 `json:"max_temperature"`
	CurrentTemperature float64 `json:"current_temperature"`
}

type GPUMetrics struct {
	MaxFrequency       float64 `json:"max_frequency"`
	CurrentFrequency   float64 `json:"current_frequency"`
	CurrentTemperature float64 `json:"current_temperature"`
	MaxTemperature     float64 `json:"max_temperature"`
}

type MemoryMetrics struct {
	MaxCapacity  float64 `json:"max_capacity"`
	UsedCapacity float64 `json:"used_capacity"`
}

type ScreenMetrics struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	RefreshRate float64 `json:"refresh_rate"`
}

type DiskMetrics struct {
	TotalCapacity     float64 `json:"total_capacity"`
	UsedCapacity      float64 `json:"used_capacity"`
	AvailableCapacity float64 `json:"available_capacity"`
}

type SystemMetrics struct {
	CPU       CPUMetrics    `json:"cpu"`
	GPU       GPUMetrics    `json:"gpu"`
	Memory    MemoryMetrics `json:"memory"`
	Screen    ScreenMetrics `json:"screen"`
	Uptime    float64       `json:"uptime"`
	Disk      DiskMetrics   `json:"disk"`
	Timestamp time.Time     `json:"timestamp"`
}

type SystemDBusMetrics struct {
	CPU                CPUMetrics    `json:"cpu"`
	GPU                GPUMetrics    `json:"gpu"`
	Memory             MemoryMetrics `json:"memory"`
	Screen             ScreenMetrics `json:"screen"`
	Uptime             float64       `json:"uptime"`
	Disk               DiskMetrics   `json:"disk"`
	TimestampUnixMilli int64         `json:"timestamp"`
}

func (p *SystemMetrics) DBus() *SystemDBusMetrics {
	return &SystemDBusMetrics{
		CPU:                p.CPU,
		GPU:                p.GPU,
		Memory:             p.Memory,
		Screen:             p.Screen,
		Uptime:             p.Uptime,
		Disk:               p.Disk,
		TimestampUnixMilli: p.Timestamp.UnixMilli(),
	}
}

type HandlerFunc func(metrics *SystemMetrics)

//go:generate mockgen -source=monitor.go -destination=../mocks/metric.go -package=mocks -mock_names=Monitor=MockMonitor
type Monitor interface {
	// AddHandler adds a callback for system metrics updates
	AddHandler(f HandlerFunc)

	// RemoveHandler removes a callback for system metrics updates
	RemoveHandler(f HandlerFunc)

	// Start starts the system metrics monitor
	Start()

	// Stop stops the system metrics monitor
	Stop()

	// LastMetrics gets the last system metrics
	LastMetrics() *SystemMetrics
}

type monitor struct {
	sync.Mutex

	ctx         context.Context
	logger      *zap.Logger
	lastMetrics *SystemMetrics
	handlers    []HandlerFunc
	doneChan    chan struct{}
	systemType  SystemType

	// Dependencies
	clock    wrapper.Clock
	os       wrapper.OS
	json     wrapper.JSON
	strconv  wrapper.Strconv
	filepath wrapper.Filepath
	exec     wrapper.Exec
}

func NewMonitor(ctx context.Context, logger *zap.Logger, clock wrapper.Clock, os wrapper.OS, json wrapper.JSON, strconv wrapper.Strconv, filepath wrapper.Filepath, exec wrapper.Exec) Monitor {
	m := &monitor{
		ctx:         ctx,
		logger:      logger,
		handlers:    []HandlerFunc{},
		doneChan:    make(chan struct{}),
		lastMetrics: &SystemMetrics{},
		clock:       clock,
		os:          os,
		json:        json,
		strconv:     strconv,
		filepath:    filepath,
		exec:        exec,
	}
	m.systemType = m.detectCPUType()
	return m
}

func (m *monitor) LastMetrics() *SystemMetrics {
	m.Lock()
	defer m.Unlock()

	return m.lastMetrics
}

func (m *monitor) Start() {
	go m.run()
}

func (m *monitor) run() {
	m.logger.Info("System monitor started in the background")

	go m.tick(m.ctx, 2*time.Second, m.monitorCPUFrequency, m.monitorGPUFreq, m.monitorMemory, m.monitorDisk)
	go m.tick(m.ctx, 4*time.Second, m.monitorCPUTemperature)
	go m.tick(m.ctx, 30*time.Second, m.monitorUptime)
	go m.tick(m.ctx, 60*time.Second, m.monitorScreen)
}

func (m *monitor) tick(ctx context.Context, interval time.Duration, fns ...func(ctx context.Context) error) {
	ticker := m.clock.NewTicker(interval)
	defer ticker.Stop()

	mf := func(fns ...func(ctx context.Context) error) {
		t := m.clock.Now()
		var updated bool
		for _, fn := range fns {
			err := fn(ctx)
			if err != nil {
				m.logger.Error("Failed to monitor system resources", zap.Error(err))
				continue
			}
			updated = true
		}
		if updated {
			m.Lock()
			m.lastMetrics.Timestamp = t
			m.Unlock()
			m.notifyHandlers(ctx, m.lastMetrics)
		}
	}

	// Run the functions once to get the initial metrics
	mf(fns...)

	for {
		select {
		case <-m.doneChan:
			return
		case <-ctx.Done():
			return
		case <-ticker.C():
			mf(fns...)
		}
	}
}

func (m *monitor) detectCPUType() SystemType {
	file, err := m.os.Open("/proc/cpuinfo")
	if err != nil {
		return SystemTypeIntel
	}
	defer func() {
		if err := file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close file: %v\n", err)
		}
	}()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.ToLower(scanner.Text())
		if strings.Contains(line, "vendor_id") {
			if strings.Contains(line, "genuineintel") {
				return SystemTypeIntel
			} else if strings.Contains(line, "authenticamd") {
				return SystemTypeAMD
			}
		}
	}
	return SystemTypeIntel
}

func (m *monitor) monitorCPUFrequency(_ context.Context) error {
	// Find all CPU frequency files
	cpuFreqFiles, err := m.filepath.Glob("/sys/devices/system/cpu/cpu*/cpufreq/scaling_cur_freq")
	if err != nil {
		return err
	}

	if len(cpuFreqFiles) == 0 {
		return fmt.Errorf("no CPU frequency files found")
	}

	// Get current frequency (average of all cores)
	var sum int64
	for _, file := range cpuFreqFiles {
		//nolint:gosec
		data, err := m.os.ReadFile(file)
		if err != nil {
			return err
		}

		freq, err := m.strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return err
		}
		sum += freq
	}
	current := float64(sum) / float64(len(cpuFreqFiles)) / 1000.0 // Convert to MHz
	m.Lock()
	m.lastMetrics.CPU.CurrentFrequency = current
	m.Unlock()

	// Get max frequency - look for cpuinfo_max_freq
	maxFreqFiles, _ := m.filepath.Glob("/sys/devices/system/cpu/cpu*/cpufreq/cpuinfo_max_freq")
	if len(maxFreqFiles) > 0 {
		data, err := m.os.ReadFile(maxFreqFiles[0])
		if err != nil {
			return err
		}

		maxFreq, err := m.strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return err
		}
		max := float64(maxFreq) / 1000.0 // Convert to MHz
		m.Lock()
		m.lastMetrics.CPU.MaxFrequency = max
		m.Unlock()
	}

	return nil
}

func (m *monitor) monitorCPUTemperature(ctx context.Context) error {
	switch m.systemType {
	case SystemTypeIntel:
		if err := m.monitorIntelTemperature(ctx); err != nil {
			return err
		}
	case SystemTypeAMD:
		if err := m.monitorAMDCPUTemperature(ctx); err != nil {
			return err
		}
		if err := m.monitorAMDGPUTemperature(ctx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported system type")
	}
	CPUTemperatureCelsius.Set(m.lastMetrics.CPU.CurrentTemperature)
	return nil
}

func (m *monitor) monitorIntelTemperature(ctx context.Context) error {
	cmd := m.exec.CommandContext(ctx, "sensors", "-u")
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
	output, err := cmd.Output()
	if err != nil {
		m.logger.Error("Failed to get CPU temperature", zap.String("stderr", stderr.String()), zap.Error(err))
		return err
	}

	// Parse the output
	lines := strings.Split(string(output), "\n")
	var inPackage bool
	for _, line := range lines {
		if strings.HasPrefix(line, "Package id 0:") {
			inPackage = true
			continue
		}
		if inPackage && line == "" {
			inPackage = false
		}
		if inPackage && strings.Contains(line, "temp1_input:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				m.logger.Error("Failed to parse current CPU temperature", zap.String("line", line))
				continue
			}
			current, err := m.strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return err
			}
			m.Lock()
			m.lastMetrics.CPU.CurrentTemperature = current
			m.lastMetrics.GPU.CurrentTemperature = current
			m.Unlock()
		}
		if inPackage && strings.Contains(line, "temp1_max:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				m.logger.Error("Failed to parse max CPU temperature", zap.String("line", line))
				continue
			}
			max, err := m.strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return err
			}
			m.Lock()
			m.lastMetrics.CPU.MaxTemperature = max
			m.lastMetrics.GPU.MaxTemperature = max
			m.Unlock()
		}
	}

	return nil
}

func (m *monitor) monitorAMDCPUTemperature(ctx context.Context) error {
	cmd := m.exec.CommandContext(ctx, "sensors", "-u")
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
	output, err := cmd.Output()
	if err != nil {
		m.logger.Error("Failed to get CPU temperature", zap.String("stderr", stderr.String()), zap.Error(err))
		return err
	}

	// Parse the output for AMD systems
	lines := strings.Split(string(output), "\n")
	var inK10Temp bool

	for _, line := range lines {
		// Look for k10temp section (AMD CPU temperature sensor)
		if strings.HasPrefix(line, "k10temp-pci-") {
			inK10Temp = true
			continue
		}

		// End of section
		if inK10Temp && line == "" {
			inK10Temp = false
		}

		// Parse Tctl temperature (AMD CPU temperature)
		if inK10Temp && strings.Contains(line, "temp1_input:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				m.logger.Error("Failed to parse current CPU temperature", zap.String("line", line))
				continue
			}
			current, err := m.strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return err
			}
			m.Lock()
			m.lastMetrics.CPU.CurrentTemperature = current
			m.Unlock()
		}

		// AMD typically doesn't expose max temperature via sensors
		// Set a typical safe maximum for AMD Ryzen™ 7 5825U
		m.Lock()
		m.lastMetrics.CPU.MaxTemperature = 95.0 // AMD Ryzen™ 7 5825U Max Temp
		m.Unlock()
	}

	return nil
}

func (m *monitor) monitorAMDGPUTemperature(ctx context.Context) error {
	cmd := m.exec.CommandContext(ctx, "sensors", "-u")
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
	output, err := cmd.Output()
	if err != nil {
		return err
	}

	lines := strings.Split(string(output), "\n")
	var inAMDGPU bool

	for _, line := range lines {
		// Look for amdgpu section
		if strings.HasPrefix(line, "amdgpu-pci-") {
			inAMDGPU = true
			continue
		}

		// End of section
		if inAMDGPU && line == "" {
			inAMDGPU = false
		}

		// Parse GPU temperature
		if inAMDGPU && strings.Contains(line, "temp1_input:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			temp, err := m.strconv.ParseFloat(fields[1], 64)
			if err != nil {
				continue
			}
			m.Lock()
			m.lastMetrics.GPU.CurrentTemperature = temp
			m.lastMetrics.GPU.MaxTemperature = 95.0 // AMD Ryzen™ 7 5825U Max Temp
			m.Unlock()
		}
	}

	return nil
}
func (m *monitor) monitorGPUFreq(ctx context.Context) error {
	switch m.systemType {
	case SystemTypeIntel:
		return m.monitorIntelGPUFreq(ctx)
	case SystemTypeAMD:
		return m.monitorAMDGPUFreq(ctx)
	default:
		return fmt.Errorf("unsupported system type")
	}
}

func (m *monitor) monitorIntelGPUFreq(ctx context.Context) error {
	// Get the current frequency
	cmd := m.exec.CommandContext(ctx, "timeout", "1s", "sudo", "intel_gpu_top", "-J", "-s", "1000")
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
	output, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 124 {
		if err != nil {
			m.logger.Error("Failed to get Intel GPU frequency", zap.String("stderr", stderr.String()), zap.Error(err))
		}
		return err
	}

	outputString := string(output)
	if strings.HasPrefix(outputString, "[") && !strings.HasSuffix(outputString, "]") {
		outputString = outputString + "]"
	}

	var result []struct {
		Frequency struct {
			Actual float64 `json:"actual"`
		} `json:"frequency"`
	}
	err = m.json.Unmarshal([]byte(outputString), &result)
	if err != nil {
		return err
	}
	if len(result) == 0 {
		return fmt.Errorf("no GPU frequency found")
	}

	current := result[0].Frequency.Actual
	m.Lock()
	m.lastMetrics.GPU.CurrentFrequency = current
	m.Unlock()

	// Discover the card name using `ls /sys/class/drm/`
	cmd = m.exec.CommandContext(ctx, "ls", "/sys/class/drm/")
	cmd.SetStderr(&stderr)
	output, err = cmd.Output()
	if err != nil {
		m.logger.Error("Failed to get Intel GPU frequency", zap.String("stderr", stderr.String()), zap.Error(err))
		return err
	}
	lines := strings.Split(string(output), "\n")
	var card string
	for _, line := range lines {
		regex := regexp.MustCompile(`^card[0-9]+`)
		if regex.MatchString(line) {
			card = regex.FindString(line)
			break
		}
	}

	// Get the max frequency
	//nolint:gosec
	cmd = m.exec.CommandContext(ctx, "cat", "/sys/class/drm/"+card+"/gt_max_freq_mhz")
	cmd.SetStderr(&stderr)
	output, err = cmd.Output()
	if err != nil {
		m.logger.Error("Failed to get Intel GPU frequency", zap.String("stderr", stderr.String()), zap.Error(err))
		return err
	}
	max, err := m.strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		return err
	}
	m.Lock()
	m.lastMetrics.GPU.MaxFrequency = max
	m.Unlock()

	return nil
}

func (m *monitor) monitorAMDGPUFreq(ctx context.Context) error {
	cmd := m.exec.CommandContext(ctx, "sensors", "-u")
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
	output, err := cmd.Output()
	if err != nil {
		return err
	}

	lines := strings.Split(string(output), "\n")
	var inAMDGPU bool

	for _, line := range lines {
		// Look for amdgpu section
		if strings.HasPrefix(line, "amdgpu-pci-") {
			inAMDGPU = true
			continue
		}

		// End of section
		if inAMDGPU && line == "" {
			inAMDGPU = false
		}

		// Parse sclk frequency (AMD GPU clock)
		if inAMDGPU && strings.Contains(line, "freq1_input:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			freq, err := m.strconv.ParseFloat(fields[1], 64)
			if err != nil {
				continue
			}
			// Convert from Hz to MHz
			current := freq / 1000000.0
			m.Lock()
			m.lastMetrics.GPU.CurrentFrequency = current
			m.Unlock()
		}
		m.Lock()
		m.lastMetrics.GPU.MaxFrequency = 2000.0 // 2000 MHz max for AMD Ryzen™ 7 5825U
		m.Unlock()
	}

	return nil
}

func (m *monitor) monitorMemory(ctx context.Context) error {
	file, err := m.os.Open("/proc/meminfo")
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(file)
	var memTotal, memAvailable int64

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				memTotal, _ = m.strconv.ParseInt(fields[1], 10, 64)
			}
		} else if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				memAvailable, _ = m.strconv.ParseInt(fields[1], 10, 64)
			}
		}
	}

	memUsed := memTotal - memAvailable
	total := float64(memTotal) / 1024.0 // Convert to MB
	used := float64(memUsed) / 1024.0   // Convert to MB

	m.Lock()
	m.lastMetrics.Memory.UsedCapacity = used
	m.lastMetrics.Memory.MaxCapacity = total
	m.Unlock()

	return nil
}

func (m *monitor) monitorUptime(_ context.Context) error {
	// Read the uptime file
	data, err := m.os.ReadFile("/proc/uptime")
	if err != nil {
		return err
	}

	// Parse the uptime value (first value in the file)
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return fmt.Errorf("unexpected format in /proc/uptime")
	}

	// Convert uptime to float (seconds)
	uptimeSec, err := m.strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return err
	}

	m.Lock()
	m.lastMetrics.Uptime = uptimeSec
	m.Unlock()

	CPUUptimeSeconds.Set(uptimeSec)

	return nil
}

func (m *monitor) monitorScreen(ctx context.Context) error {
	// Resolution and refresh rate
	cmd := m.exec.CommandContext(ctx, "wlr-randr")
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
	output, err := cmd.Output()
	if err != nil {
		m.logger.Error("Failed to get screen metrics", zap.String("stderr", stderr.String()), zap.Error(err))
		return err
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "current") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				return fmt.Errorf("unexpected format in wlr-randr output")
			}

			// resolution
			dimensions := strings.Split(fields[0], "x")
			if len(dimensions) != 2 {
				return fmt.Errorf("unexpected format in wlr-randr output")
			}
			width, err := m.strconv.Atoi(dimensions[0])
			if err != nil {
				return err
			}
			height, err := m.strconv.Atoi(dimensions[1])
			if err != nil {
				return err
			}

			// refresh rate
			refreshRate, err := m.strconv.ParseFloat(fields[2], 64)
			if err != nil {
				return err
			}

			m.Lock()
			m.lastMetrics.Screen.Width = width
			m.lastMetrics.Screen.Height = height
			m.lastMetrics.Screen.RefreshRate = refreshRate
			m.Unlock()

			break
		}
	}

	return nil
}

func (m *monitor) monitorDisk(ctx context.Context) error {
	// Get total/used capacity
	total, used, available, err := m.getDiskStats(ctx)
	if err != nil {
		return err
	}

	m.Lock()
	m.lastMetrics.Disk.TotalCapacity = total
	m.lastMetrics.Disk.UsedCapacity = used
	m.lastMetrics.Disk.AvailableCapacity = available
	m.Unlock()

	return nil
}

func (m *monitor) getDiskStats(ctx context.Context) (total, used, available float64, err error) {
	cmd := m.exec.CommandContext(ctx, "df", "-k", "/")
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
	output, err := cmd.Output()
	if err != nil {
		m.logger.Error("Failed to get disk stats", zap.String("stderr", stderr.String()), zap.Error(err))
		return 0, 0, 0, err
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return 0, 0, 0, fmt.Errorf("unexpected format in df output")
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return 0, 0, 0, fmt.Errorf("unexpected format in df output")
	}

	total, err = m.strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	used, err = m.strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	available, err = m.strconv.ParseFloat(fields[3], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	return total, used, available, nil
}

func (m *monitor) notifyHandlers(ctx context.Context, metrics *SystemMetrics) {
	m.Lock()
	handlers := make([]HandlerFunc, len(m.handlers))
	copy(handlers, m.handlers)
	m.Unlock()

	for _, handler := range handlers {
		go func(h HandlerFunc) {
			select {
			case <-ctx.Done():
				return
			case <-m.doneChan:
				return
			default:
				h(metrics)
			}
		}(handler)
	}
}

func (m *monitor) AddHandler(handler HandlerFunc) {
	m.Lock()
	defer m.Unlock()

	m.handlers = append(m.handlers, handler)
}

func (m *monitor) RemoveHandler(handler HandlerFunc) {
	m.Lock()
	defer m.Unlock()

	for i, h := range m.handlers {
		if fmt.Sprintf("%p", h) == fmt.Sprintf("%p", handler) {
			m.handlers = append(m.handlers[:i], m.handlers[i+1:]...)
			return
		}
	}
}

func (m *monitor) Stop() {
	select {
	case <-m.doneChan:
		return
	default:
		close(m.doneChan)
	}

	m.logger.Info("System monitor stopped")
}
