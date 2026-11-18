package mocks

import (
	"context"
	"fmt"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
)

// CommandSender keeps the existing admission/policy test fixtures readable while
// their transport is removed. It serializes only into a mock, never a browser.
// Production packages must not import mocks. New transport tests use the native
// protocol directly; remove this fixture adapter as old payload tests are ported.
type CommandSender struct{ CDP cdp.CDP }

func (s CommandSender) Ready(context.Context) bool { return s.CDP != nil && s.CDP.Initialized() }
func (s CommandSender) Send(_ context.Context, command commands.Command) (interface{}, error) {
	if s.CDP == nil {
		return nil, fmt.Errorf("player unavailable")
	}
	data, err := command.JSON()
	if err != nil {
		return nil, err
	}
	params := map[string]interface{}{"expression": fmt.Sprintf("window.handleCDPRequest(%s)", data)}
	if command.Type == commands.CMD_GET_CONTENT_POLICY || command.Type == commands.CMD_SET_CONTENT_POLICY {
		params["awaitPromise"] = true
		params["returnByValue"] = true
	}
	if command.Type == "mintPairingDisplay" {
		params["returnByValue"] = true
		params["awaitPromise"] = true
		return s.CDP.NoLogSend(cdp.METHOD_EVALUATE, params)
	}
	return s.CDP.Send(cdp.METHOD_EVALUATE, params)
}
