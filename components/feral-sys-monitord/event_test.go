package main

import "testing"

func TestClassifyAMDGPUJournalLine(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantEvent Event
		wantOK    bool
	}{
		{
			name:      "ring timeout reports hang",
			line:      "kernel: [drm:amdgpu_job_timedout [amdgpu]] *ERROR* ring gfx_0.0.0 timeout, signaled seq=123, emitted seq=125",
			wantEvent: EVENT_GPU_HANGING,
			wantOK:    true,
		},
		{
			name:      "reset begin reports hang",
			line:      "kernel: amdgpu 0000:63:00.0: amdgpu: GPU reset begin!",
			wantEvent: EVENT_GPU_HANGING,
			wantOK:    true,
		},
		{
			name:      "reset succeeded reports recovery",
			line:      "kernel: amdgpu 0000:63:00.0: amdgpu: GPU reset(1) succeeded!",
			wantEvent: EVENT_GPU_RECOVER,
			wantOK:    true,
		},
		{
			name:   "unrelated amdgpu line is ignored",
			line:   "kernel: amdgpu 0000:63:00.0: amdgpu: Fetched VBIOS from VFCT",
			wantOK: false,
		},
		{
			name:   "ring error without timeout is ignored",
			line:   "kernel: [drm:amdgpu_ib_ring_tests [amdgpu]] *ERROR* ring gfx_0.0.0 test failed (-110)",
			wantOK: false,
		},
		{
			name:   "timeout without ring error is ignored",
			line:   "kernel: amdgpu: SMU: I'm not done with your command: SMN_C2PMSG_66:0x0000000E, timeout waiting",
			wantOK: false,
		},
		{
			name:   "reset failed is not recovery",
			line:   "kernel: amdgpu 0000:63:00.0: amdgpu: GPU reset(2) failed",
			wantOK: false,
		},
		{
			name:   "empty line is ignored",
			line:   "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, ok := classifyAMDGPUJournalLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("classifyAMDGPUJournalLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && event != tt.wantEvent {
				t.Fatalf("classifyAMDGPUJournalLine(%q) event = %q, want %q", tt.line, event, tt.wantEvent)
			}
		})
	}
}
