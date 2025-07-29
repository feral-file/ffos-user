package metric

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
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

type SysMetrics struct {
	CPU       CPUMetrics    `json:"cpu"`
	GPU       GPUMetrics    `json:"gpu"`
	Memory    MemoryMetrics `json:"memory"`
	Screen    ScreenMetrics `json:"screen"`
	Uptime    float64       `json:"uptime"`
	Disk      DiskMetrics   `json:"disk"`
	Timestamp time.Time     `json:"timestamp"`
}

type SysDBusMetrics struct {
	CPU                CPUMetrics    `json:"cpu"`
	GPU                GPUMetrics    `json:"gpu"`
	Memory             MemoryMetrics `json:"memory"`
	Screen             ScreenMetrics `json:"screen"`
	Uptime             float64       `json:"uptime"`
	Disk               DiskMetrics   `json:"disk"`
	TimestampUnixMilli int64         `json:"timestamp"`
}

func (p *SysMetrics) DBus() *SysDBusMetrics {
	return &SysDBusMetrics{
		CPU:                p.CPU,
		GPU:                p.GPU,
		Memory:             p.Memory,
		Screen:             p.Screen,
		Uptime:             p.Uptime,
		Disk:               p.Disk,
		TimestampUnixMilli: p.Timestamp.UnixMilli(),
	}
}

type MonitorHandler func(metrics *SysMetrics)

type SysResMonitor struct {
	sync.Mutex

	ctx         context.Context
	logger      *zap.Logger
	lastMetrics *SysMetrics
	handlers    []MonitorHandler
	doneChan    chan struct{}
}

func NewSysResMonitor(ctx context.Context, logger *zap.Logger) *SysResMonitor {
	return &SysResMonitor{
		ctx:         ctx,
		logger:      logger,
		handlers:    []MonitorHandler{},
		doneChan:    make(chan struct{}),
		lastMetrics: &SysMetrics{},
	}
}

func (p *SysResMonitor) LastMetrics() *SysMetrics {
	p.Lock()
	defer p.Unlock()

	return p.lastMetrics
}

func (p *SysResMonitor) Start() {
	go p.run()
}

func (p *SysResMonitor) run() {
	p.logger.Info("SysResMonitor started in the background")

	go p.tick(p.ctx, 2*time.Second, p.monitorCPUFrequency, p.monitorGPUFreq, p.monitorMemory, p.monitorDisk)
	go p.tick(p.ctx, 4*time.Second, p.monitorCPUTemperature)
	go p.tick(p.ctx, 30*time.Second, p.monitorUptime)
	go p.tick(p.ctx, 60*time.Second, p.monitorScreen)
}

func (p *SysResMonitor) tick(ctx context.Context, interval time.Duration, fns ...func(ctx context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	mf := func(fns ...func(ctx context.Context) error) {
		t := time.Now()
		var updated bool
		for _, fn := range fns {
			err := fn(ctx)
			if err != nil {
				p.logger.Error("Failed to monitor system resources", zap.Error(err))
				continue
			}
			updated = true
		}
		if updated {
			p.Lock()
			p.lastMetrics.Timestamp = t
			p.Unlock()
			p.notifyHandlers(ctx, p.lastMetrics)
		}
	}

	// Run the functions once to get the initial metrics
	mf(fns...)

	for {
		select {
		case <-p.doneChan:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			mf(fns...)
		}
	}
}

func (p *SysResMonitor) monitorCPUFrequency(_ context.Context) error {
	// Find all CPU frequency files
	cpuFreqFiles, err := filepath.Glob("/sys/devices/system/cpu/cpu*/cpufreq/scaling_cur_freq")
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
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}

		freq, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return err
		}
		sum += freq
	}
	current := float64(sum) / float64(len(cpuFreqFiles)) / 1000.0 // Convert to MHz
	p.Lock()
	p.lastMetrics.CPU.CurrentFrequency = current
	p.Unlock()

	// Get max frequency - look for cpuinfo_max_freq
	maxFreqFiles, _ := filepath.Glob("/sys/devices/system/cpu/cpu*/cpufreq/cpuinfo_max_freq")
	if len(maxFreqFiles) > 0 {
		data, err := os.ReadFile(maxFreqFiles[0])
		if err != nil {
			return err
		}

		maxFreq, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return err
		}
		max := float64(maxFreq) / 1000.0 // Convert to MHz
		p.Lock()
		p.lastMetrics.CPU.MaxFrequency = max
		p.Unlock()
	}

	return nil
}

func (p *SysResMonitor) monitorCPUTemperature(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "sensors", "-u")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		p.logger.Error("Failed to get CPU temperature", zap.String("stderr", stderr.String()), zap.Error(err))
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
				p.logger.Error("Failed to parse current CPU temperature", zap.String("line", line))
				continue
			}
			current, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return err
			}
			p.Lock()
			p.lastMetrics.CPU.CurrentTemperature = current
			p.lastMetrics.GPU.CurrentTemperature = current
			p.Unlock()
		}
		if inPackage && strings.Contains(line, "temp1_max:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				p.logger.Error("Failed to parse max CPU temperature", zap.String("line", line))
				continue
			}
			max, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return err
			}
			p.Lock()
			p.lastMetrics.CPU.MaxTemperature = max
			p.lastMetrics.GPU.MaxTemperature = max
			p.Unlock()
		}
	}

	return nil
}

func (p *SysResMonitor) monitorGPUFreq(ctx context.Context) error {
	// Get the current frequency
	cmd := exec.CommandContext(ctx, "timeout", "1s", "sudo", "intel_gpu_top", "-J", "-s", "1000")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 124 {
		if err != nil {
			p.logger.Error("Failed to get Intel GPU frequency", zap.String("stderr", stderr.String()), zap.Error(err))
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
	err = json.Unmarshal([]byte(outputString), &result)
	if err != nil {
		return err
	}
	if len(result) == 0 {
		return fmt.Errorf("no GPU frequency found")
	}

	current := result[0].Frequency.Actual
	p.Lock()
	p.lastMetrics.GPU.CurrentFrequency = current
	p.Unlock()

	// Discover the card name using `ls /sys/class/drm/`
	cmd = exec.CommandContext(ctx, "ls", "/sys/class/drm/")
	cmd.Stderr = &stderr
	output, err = cmd.Output()
	if err != nil {
		p.logger.Error("Failed to get Intel GPU frequency", zap.String("stderr", stderr.String()), zap.Error(err))
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
	cmd = exec.CommandContext(ctx, "cat", "/sys/class/drm/"+card+"/gt_max_freq_mhz")
	cmd.Stderr = &stderr
	output, err = cmd.Output()
	if err != nil {
		p.logger.Error("Failed to get Intel GPU frequency", zap.String("stderr", stderr.String()), zap.Error(err))
		return err
	}
	max, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		return err
	}
	p.Lock()
	p.lastMetrics.GPU.MaxFrequency = max
	p.Unlock()

	return nil
}

func (p *SysResMonitor) monitorMemory(ctx context.Context) error {
	file, err := os.Open("/proc/meminfo")
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
				memTotal, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		} else if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				memAvailable, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
	}

	memUsed := memTotal - memAvailable
	total := float64(memTotal) / 1024.0 // Convert to MB
	used := float64(memUsed) / 1024.0   // Convert to MB

	p.Lock()
	p.lastMetrics.Memory.UsedCapacity = used
	p.lastMetrics.Memory.MaxCapacity = total
	p.Unlock()

	return nil
}

func (p *SysResMonitor) monitorUptime(_ context.Context) error {
	// Read the uptime file
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return err
	}

	// Parse the uptime value (first value in the file)
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return fmt.Errorf("unexpected format in /proc/uptime")
	}

	// Convert uptime to float (seconds)
	uptimeSec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return err
	}

	p.Lock()
	p.lastMetrics.Uptime = uptimeSec
	p.Unlock()

	return nil
}

func (p *SysResMonitor) monitorScreen(ctx context.Context) error {
	// Resolution and refresh rate
	cmd := exec.CommandContext(ctx, "wlr-randr")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		p.logger.Error("Failed to get screen metrics", zap.String("stderr", stderr.String()), zap.Error(err))
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
			width, err := strconv.Atoi(dimensions[0])
			if err != nil {
				return err
			}
			height, err := strconv.Atoi(dimensions[1])
			if err != nil {
				return err
			}

			// refresh rate
			refreshRate, err := strconv.ParseFloat(fields[2], 64)
			if err != nil {
				return err
			}

			p.Lock()
			p.lastMetrics.Screen.Width = width
			p.lastMetrics.Screen.Height = height
			p.lastMetrics.Screen.RefreshRate = refreshRate
			p.Unlock()

			break
		}
	}

	return nil
}

func (p *SysResMonitor) monitorDisk(ctx context.Context) error {
	// Get total/used capacity
	total, used, available, err := p.getDiskStats(ctx)
	if err != nil {
		return err
	}

	p.Lock()
	p.lastMetrics.Disk.TotalCapacity = total
	p.lastMetrics.Disk.UsedCapacity = used
	p.lastMetrics.Disk.AvailableCapacity = available
	p.Unlock()

	return nil
}

func (p *SysResMonitor) getDiskStats(ctx context.Context) (total, used, available float64, err error) {
	cmd := exec.CommandContext(ctx, "df", "-k", "/")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		p.logger.Error("Failed to get disk stats", zap.String("stderr", stderr.String()), zap.Error(err))
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

	total, err = strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	used, err = strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	available, err = strconv.ParseFloat(fields[3], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	return total, used, available, nil
}

func (p *SysResMonitor) notifyHandlers(ctx context.Context, metrics *SysMetrics) {
	p.Lock()
	handlers := make([]MonitorHandler, len(p.handlers))
	copy(handlers, p.handlers)
	p.Unlock()

	for _, handler := range handlers {
		go func(h MonitorHandler) {
			select {
			case <-ctx.Done():
				return
			case <-p.doneChan:
				return
			default:
				h(metrics)
			}
		}(handler)
	}
}

func (p *SysResMonitor) OnMonitor(handler MonitorHandler) {
	p.Lock()
	defer p.Unlock()

	p.handlers = append(p.handlers, handler)
}

func (p *SysResMonitor) RemoveMonitorHandler(handler MonitorHandler) {
	p.Lock()
	defer p.Unlock()

	for i, h := range p.handlers {
		if fmt.Sprintf("%p", h) == fmt.Sprintf("%p", handler) {
			p.handlers = append(p.handlers[:i], p.handlers[i+1:]...)
			return
		}
	}
}

func (p *SysResMonitor) Stop() {
	select {
	case <-p.doneChan:
		return
	default:
		close(p.doneChan)
	}

	p.logger.Info("SysResMonitor stopped")
}
