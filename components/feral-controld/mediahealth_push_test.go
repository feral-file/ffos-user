package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
)

func TestHintSender_SendsSetMediaHealthAndDecodesTheReply(t *testing.T) {
	var gotMethod, gotExpr string
	send := func(method string, params map[string]interface{}) (interface{}, error) {
		gotMethod = method
		gotExpr = params["expression"].(string)
		return map[string]interface{}{"message": map[string]interface{}{
			"ok": true, "skippedIndices": []interface{}{float64(1), float64(3)},
		}}, nil
	}
	sender := newHintSender(send, func() uint64 { return 9 })
	hints := map[string]mediahealth.Hint{"https://o/b": {State: mediahealth.StateBroken, Category: mediahealth.CategorySourceMissing}}
	skipped, err := sender(context.Background(), 4, hints)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 3}, skipped)
	assert.Equal(t, cdp.METHOD_EVALUATE, gotMethod)

	payload := strings.TrimSuffix(strings.TrimPrefix(gotExpr, "window.handleCDPRequest("), ")")
	var cmd struct {
		Command string `json:"command"`
		Request struct {
			CastSeq     int64                       `json:"castSeq"`
			MediaHealth map[string]mediahealth.Hint `json:"mediaHealth"`
		} `json:"request"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &cmd))
	assert.Equal(t, "setMediaHealth", cmd.Command)
	assert.Equal(t, int64(4), cmd.Request.CastSeq)
	assert.Equal(t, hints, cmd.Request.MediaHealth)

	// A heal is an empty map, never an absent one.
	_, err = sender(context.Background(), 4, nil)
	require.NoError(t, err)
	assert.Contains(t, gotExpr, `"mediaHealth":{}`)
}

func TestHintSender_ReplyBranches(t *testing.T) {
	reply := func(v interface{}, err error) cdpSend {
		return func(string, map[string]interface{}) (interface{}, error) { return v, err }
	}
	gen := func() uint64 { return 1 }
	cases := []struct {
		name string
		send cdpSend
		want error
	}{
		{"stale cast", reply(map[string]interface{}{"ok": false, "error": "staleCast"}, nil), mediahealth.ErrStaleCast},
		{"unknown command", reply(map[string]interface{}{"ok": false, "error": "Unknown command"}, nil), mediahealth.ErrHintPushRefused},
		{"bare legacy failure", reply(map[string]interface{}{"ok": false}, nil), mediahealth.ErrHintPushRefused},
		{"not an object", reply(nil, nil), mediahealth.ErrHintPushRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newHintSender(tc.send, gen)(context.Background(), 1, nil)
			assert.ErrorIs(t, err, tc.want)
		})
	}

	transport := errors.New("cdp not connected")
	_, err := newHintSender(reply(nil, transport), gen)(context.Background(), 1, nil)
	assert.ErrorIs(t, err, transport)
	assert.NotErrorIs(t, err, mediahealth.ErrHintPushRefused, "transport weather is retried, never parked")

	// A reply that crossed a page generation change came from a page that
	// is gone: neither delivered nor refused.
	g := uint64(1)
	moving := func() uint64 { g++; return g }
	_, err = newHintSender(reply(map[string]interface{}{"ok": true}, nil), moving)(context.Background(), 1, nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, mediahealth.ErrHintPushRefused)
	assert.NotErrorIs(t, err, mediahealth.ErrStaleCast)
}
