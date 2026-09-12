package sigverify_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

// TestToastFor is the whole (mode, status) → notice policy table
// (feral-file/ffos-user#307): silent never toasts; notify surfaces a verdict
// it has (invalid/unsigned) and nothing else; strict toasts "rejected" for
// anything not proven valid, including the verdict-less cached copy (status
// ""); valid never toasts.
func TestToastFor(t *testing.T) {
	type want struct {
		notice sigverify.Notice
		show   bool
	}
	cases := []struct {
		mode   sigverify.Mode
		status sigverify.Status
		want   want
	}{
		{sigverify.ModeSilent, sigverify.StatusValid, want{"", false}},
		{sigverify.ModeSilent, sigverify.StatusInvalid, want{"", false}},
		{sigverify.ModeSilent, sigverify.StatusUnsigned, want{"", false}},
		{sigverify.ModeSilent, "", want{"", false}},

		{sigverify.ModeNotify, sigverify.StatusValid, want{"", false}},
		{sigverify.ModeNotify, sigverify.StatusInvalid, want{sigverify.NoticeInvalid, true}},
		{sigverify.ModeNotify, sigverify.StatusUnsigned, want{sigverify.NoticeUnsigned, true}},
		{sigverify.ModeNotify, "", want{"", false}},

		{sigverify.ModeStrict, sigverify.StatusValid, want{"", false}},
		{sigverify.ModeStrict, sigverify.StatusInvalid, want{sigverify.NoticeRejected, true}},
		{sigverify.ModeStrict, sigverify.StatusUnsigned, want{sigverify.NoticeRejected, true}},
		{sigverify.ModeStrict, "", want{sigverify.NoticeRejected, true}},
	}
	for _, c := range cases {
		notice, show := sigverify.ToastFor(c.mode, c.status)
		assert.Equal(t, c.want.show, show, "%s/%q show", c.mode, c.status)
		assert.Equal(t, c.want.notice, notice, "%s/%q notice", c.mode, c.status)
	}
}

// TestToastFor_NoticesMatchTheManifestStates: the notice constants are the
// exact strings ff-player's playerToast contract lists; a drift here is a
// wire break.
func TestToastFor_NoticesMatchTheManifestStates(t *testing.T) {
	assert.Equal(t, "signature_invalid", string(sigverify.NoticeInvalid))
	assert.Equal(t, "signature_unsigned", string(sigverify.NoticeUnsigned))
	assert.Equal(t, "signature_rejected", string(sigverify.NoticeRejected))
}
