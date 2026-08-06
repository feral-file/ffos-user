package refresher

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStartupEscalation_Record pins startupEscalation's transitions: forceCast
// is a symmetric latch that sets on a full rejection streak and clears the
// instant that streak's justification goes away (see the type doc for why
// asymmetry there was a bug).
func TestStartupEscalation_Record(t *testing.T) {
	tests := []struct {
		name        string
		errs        []error
		wantForce   bool
		wantRejects int
	}{
		{
			name:        "three rejections escalate to forceCast",
			errs:        []error{errPlayerRejectedRefresh, errPlayerRejectedRefresh, errPlayerRejectedRefresh},
			wantForce:   true,
			wantRejects: 3,
		},
		{
			name:        "two rejections then a success never escalate",
			errs:        []error{errPlayerRejectedRefresh, errPlayerRejectedRefresh, nil},
			wantForce:   false,
			wantRejects: 0,
		},
		{
			name:        "two rejections then a different error resets without escalating",
			errs:        []error{errPlayerRejectedRefresh, errPlayerRejectedRefresh, errCDPNotReady},
			wantForce:   false,
			wantRejects: 0,
		},
		{
			name: "three rejections escalate, then success clears forceCast",
			errs: []error{
				errPlayerRejectedRefresh, errPlayerRejectedRefresh, errPlayerRejectedRefresh,
				nil,
			},
			wantForce:   false,
			wantRejects: 0,
		},
		{
			name: "forceCast set, then a CDP error clears it: the streak's justification is gone",
			errs: []error{
				errPlayerRejectedRefresh, errPlayerRejectedRefresh, errPlayerRejectedRefresh,
				errCDPNotReady,
			},
			wantForce:   false,
			wantRejects: 0,
		},
		{
			name: "forceCast set, then an unrelated transport error also clears it",
			errs: []error{
				errPlayerRejectedRefresh, errPlayerRejectedRefresh, errPlayerRejectedRefresh,
				errors.New("dp1 fetch failed"),
			},
			wantForce:   false,
			wantRejects: 0,
		},
		{
			name: "a fresh streak after a clear must re-earn escalation from zero",
			errs: []error{
				errPlayerRejectedRefresh, errPlayerRejectedRefresh, errPlayerRejectedRefresh,
				errCDPNotReady, // clears forceCast
				errPlayerRejectedRefresh, errPlayerRejectedRefresh,
			},
			wantForce:   false,
			wantRejects: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			esc := startupEscalation{}
			for _, err := range tc.errs {
				esc.record(err)
			}
			assert.Equal(t, tc.wantForce, esc.forceCast, "forceCast")
			assert.Equal(t, tc.wantRejects, esc.consecutiveRejects, "consecutiveRejects")
		})
	}
}

// TestStartupEscalation_Record_MutationCheck pins the exact threshold
// boundary (>= startupForceCastEscalationThreshold, not > it) and confirms
// wrapped errors (errors.Is, not ==) still count as rejections — both are
// easy off-by-one/equality mutations that the table above would not catch on
// its own since every "escalate" case there already clears the threshold by
// exactly one attempt.
func TestStartupEscalation_Record_MutationCheck(t *testing.T) {
	esc := startupEscalation{}
	for i := 0; i < startupForceCastEscalationThreshold-1; i++ {
		esc.record(errPlayerRejectedRefresh)
		assert.False(t, esc.forceCast, "forceCast must stay clear before the threshold is reached")
	}
	esc.record(fmt.Errorf("wrapped: %w", errPlayerRejectedRefresh))
	assert.True(t, esc.forceCast, "the threshold-th rejection must set forceCast even wrapped")
	assert.Equal(t, startupForceCastEscalationThreshold, esc.consecutiveRejects)
}
