package qrdisplay

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
)

const pairingQRCodeURL = "file:///opt/feral/ui/mint-pairing/index.html"
const defaultDisplayURL = "http://127.0.0.1:8080/"

func ShowPairingCode(ctx context.Context, cdpClient cdp.CDP, pairingCode string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cdpClient == nil {
		return errors.New("cdp client is required")
	}
	pairingCode = strings.TrimSpace(pairingCode)
	if pairingCode == "" {
		return errors.New("pairing code is required")
	}

	u, err := url.Parse(pairingQRCodeURL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("pairing_code", pairingCode)
	u.RawQuery = q.Encode()

	_, err = cdpClient.NoLogSend("Page.navigate", map[string]interface{}{"url": u.String()})
	return err
}

func ShowDefaultDisplay(ctx context.Context, cdpClient cdp.CDP) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cdpClient == nil {
		return errors.New("cdp client is required")
	}

	_, err := cdpClient.NoLogSend("Page.navigate", map[string]interface{}{"url": defaultDisplayURL})
	return err
}
