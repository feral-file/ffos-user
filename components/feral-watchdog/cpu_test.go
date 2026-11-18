package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestThermalHoldRequiresSustainedHeatAndReaffirmsUntilCool(t *testing.T) {
	var signals []bool
	h := NewCPUHandler(zap.NewNop(), func(_ context.Context, active bool) error {
		signals = append(signals, active)
		return nil
	})
	ctx := context.Background()
	h.checkCPUTemperature(ctx, 94)
	h.highTempStartTime = time.Now().Add(-11 * time.Second)
	h.checkCPUTemperature(ctx, 90)
	require.False(t, h.highTempMonitoring, "a dip before protection interrupts the sustained-heat timer")
	h.checkCPUTemperature(ctx, 94)
	require.Empty(t, signals)
	h.highTempStartTime = time.Now().Add(-11 * time.Second)
	h.checkCPUTemperature(ctx, 94)
	h.checkCPUTemperature(ctx, 90)
	require.Equal(t, []bool{true, true}, signals, "reaffirm protection across player restarts")
	h.checkCPUTemperature(ctx, 87)
	require.Equal(t, []bool{true, true, false}, signals)
	require.False(t, h.thermalActive)
	h.checkCPUTemperature(ctx, 94)
	require.Len(t, signals, 3, "a new hot episode needs a new sustained interval")
}
