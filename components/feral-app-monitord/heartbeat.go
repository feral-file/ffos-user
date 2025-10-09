package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gowebpki/jcs"
	"go.uber.org/zap"
)

// HeartbeatData represents the data part of the payload.
type HeartbeatData struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"ts"`
	Build     string `json:"build"`

	ScreenInfo  string  `json:"screen_info"`
	CPUTemp     float64 `json:"cpu_temp"`
	CPUUsage    float64 `json:"cpu_usage"`
	GPUUsage    float64 `json:"gpu_usage"`
	MemoryUsage float64 `json:"memory_usage"`
	DiskUsage   float64 `json:"disk_usage"`
	Uptime      string  `json:"uptime"`

	Page       string `json:"page"`
	PageUptime string `json:"page_uptime"`
}

// HeartbeatPayload is the structure for the final JSON object.
type HeartbeatPayload struct {
	Data      *HeartbeatData `json:"data"`
	PublicKey string         `json:"pubkey"`
	Signature string         `json:"signature"`
}

// SendHeartbeat orchestrates the process of sending a heartbeat.
func SendHeartbeat() {
	sysMetric, err := GetSysMetrics()
	if err != nil {
		logger.Error("Failed to get sysMetric: %v", zap.Error(err))
		return
	}
	if sysMetric == nil {
		logger.Error("SysMetric is nil, cannot send heartbeat")
		return
	}
	logger.Info("Gathered sysMetric data", zap.Any("sysMetric", sysMetric))

	pageState, err := GetPageState()
	if err != nil {
		logger.Error("Failed to get pageState: %v", zap.Error(err))
		return
	}
	if pageState == nil {
		logger.Error("pageState is nil, cannot send heartbeat")
		return
	}
	logger.Info("Gathered pageState data", zap.Any("pageState", pageState))

	message := &HeartbeatData{
		ID:        pageState.ID,
		Timestamp: time.Now().UnixMilli(),
		Build:     fmt.Sprintf("%s-%s", config.Branch, config.Version),

		ScreenInfo: fmt.Sprintf(
			"%dx%d@%.0fHz",
			sysMetric.Screen.Width, sysMetric.Screen.Height, sysMetric.Screen.RefreshRate,
		),
		CPUTemp:     sysMetric.CPU.CurrentTemperature,
		CPUUsage:    safeDivide(sysMetric.CPU.CurrentFrequency, sysMetric.CPU.MaxFrequency),
		GPUUsage:    safeDivide(sysMetric.GPU.CurrentFrequency, sysMetric.GPU.MaxFrequency),
		MemoryUsage: safeDivide(sysMetric.Memory.UsedCapacity, sysMetric.Memory.MaxCapacity),
		DiskUsage:   safeDivide(sysMetric.Disk.UsedCapacity, sysMetric.Disk.TotalCapacity),
		Uptime:      humanizeDuration(int64(sysMetric.Uptime)),

		Page:       string(pageState.Page),
		PageUptime: humanizeDuration(int64(time.Since(time.Unix(pageState.PageChangedUnix, 0)).Seconds())),
	}

	var signatureHex string
	if config.Pubkey != "" {
		rawJson, err := json.Marshal(message)
		if err != nil {
			logger.Error("Failed to marshal message to JSON: %v", zap.Error(err))
			return
		}
		canonical, err := jcs.Transform(rawJson)
		if err != nil {
			logger.Error("Failed to jcs.Transform: %v", zap.Error(err))
			return
		}
		hash := sha256.Sum256(canonical)
		signatureHex, err = SignMessage(hash[:])
		if err != nil {
			logger.Error("Failed to sign message", zap.Error(err))
			return
		}
		logger.Info("Message signed successfully.")
	}

	finalPayload := &HeartbeatPayload{
		Data:      message,
		PublicKey: config.Pubkey,
		Signature: signatureHex,
	}
	finalPayloadJSON, err := json.Marshal(finalPayload)
	if err != nil {
		logger.Error("Failed to marshal final payload", zap.Error(err))
		return
	}

	if err := SendPayload(finalPayloadJSON); err != nil {
		logger.Error("Failed to send payload", zap.Error(err))
		return
	}

	logger.Info("Heartbeat sent successfully.")
}
