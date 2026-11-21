package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestInternetVerdict pins what kiosk replay's restore gate reads: offline
// only once an answer says so, the startup read never overriding a verdict
// the mediator already applied, and a later verdict always winning.
func TestInternetVerdict(t *testing.T) {
	var v internetVerdict
	assert.False(t, v.offline(), "unknown is not offline")

	v.seed(false)
	assert.True(t, v.offline(), "the startup read answers before any edge")

	v.observe(true)
	assert.False(t, v.offline(), "an applied verdict wins")

	v.seed(false)
	assert.False(t, v.offline(), "a late startup read never overrides an applied verdict")

	v.observe(false)
	assert.True(t, v.offline())
}
