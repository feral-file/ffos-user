package qrdisplay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
)

const mintPairingDisplayCommand = "mintPairingDisplay"

const (
	mintPairingDisplayPairingCode     = "pairing_code"
	mintPairingDisplayRequestReceived = "request_received"
	mintPairingDisplayCreatingToken   = "creating_token"
	mintPairingDisplayHidden          = "hidden"
)

func ShowPairingCode(ctx context.Context, cdpClient cdp.CDP, pairingCode string) error {
	pairingCode = strings.TrimSpace(pairingCode)
	if pairingCode == "" {
		return errors.New("pairing code is required")
	}
	return sendMintPairingDisplay(ctx, cdpClient, map[string]any{
		"state":       mintPairingDisplayPairingCode,
		"pairingCode": pairingCode,
	})
}

func ShowRequestReceived(ctx context.Context, cdpClient cdp.CDP, browserName string) error {
	return sendMintPairingDisplay(ctx, cdpClient, map[string]any{
		"state":       mintPairingDisplayRequestReceived,
		"browserName": strings.TrimSpace(browserName),
	})
}

func ShowCreatingToken(ctx context.Context, cdpClient cdp.CDP, browserName string) error {
	return sendMintPairingDisplay(ctx, cdpClient, map[string]any{
		"state":       mintPairingDisplayCreatingToken,
		"browserName": strings.TrimSpace(browserName),
	})
}

func ShowDefaultDisplay(ctx context.Context, cdpClient cdp.CDP) error {
	return sendMintPairingDisplay(ctx, cdpClient, map[string]any{
		"state": mintPairingDisplayHidden,
	})
}

func sendMintPairingDisplay(ctx context.Context, cdpClient cdp.CDP, request map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cdpClient == nil {
		return errors.New("cdp client is required")
	}

	payload, err := json.Marshal(map[string]any{
		"command": mintPairingDisplayCommand,
		"request": request,
	})
	if err != nil {
		return err
	}

	_, err = cdpClient.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": "window.handleCDPRequest(" + string(payload) + ")",
	})
	return err
}
