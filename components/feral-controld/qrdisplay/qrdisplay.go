package qrdisplay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	result, err := cdpClient.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    "window.handleCDPRequest(" + string(payload) + ")",
		"returnByValue": true,
	})
	if err != nil {
		return err
	}
	return validateMintPairingDisplayResult(result)
}

func validateMintPairingDisplayResult(result any) error {
	response, err := normalizeEvaluationResult(result)
	if err != nil {
		return err
	}
	ok, okType := response["ok"].(bool)
	if !okType {
		return fmt.Errorf("mint pairing display response missing ok: %v", response)
	}
	if !ok {
		return fmt.Errorf("mint pairing display rejected request: %v", response)
	}
	return nil
}

func normalizeEvaluationResult(result any) (map[string]any, error) {
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mint pairing display returned unsupported result type %T", result)
	}
	if _, hasException := resultMap["exceptionDetails"]; hasException {
		return nil, fmt.Errorf("mint pairing display evaluation raised exception: %v", resultMap["exceptionDetails"])
	}
	if _, hasOK := resultMap["ok"]; hasOK {
		return resultMap, nil
	}
	if value, ok := resultMap["value"]; ok {
		if raw, ok := value.(string); ok {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				return nil, fmt.Errorf("decode mint pairing display response: %w", err)
			}
			return decoded, nil
		}
		return normalizeEvaluationResult(value)
	}

	rawResult, hasResult := resultMap["result"]
	if !hasResult {
		return resultMap, nil
	}
	rawResultMap, ok := rawResult.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mint pairing display returned malformed Runtime.evaluate result: %v", resultMap)
	}
	if _, hasException := rawResultMap["exceptionDetails"]; hasException {
		return nil, fmt.Errorf("mint pairing display evaluation raised exception: %v", rawResultMap["exceptionDetails"])
	}
	if nested, ok := rawResultMap["result"]; ok {
		return normalizeEvaluationResult(nested)
	}
	if value, ok := rawResultMap["value"]; ok {
		if raw, ok := value.(string); ok {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				return nil, fmt.Errorf("decode mint pairing display response: %w", err)
			}
			return decoded, nil
		}
		return normalizeEvaluationResult(value)
	}
	return nil, fmt.Errorf("mint pairing display returned unsupported Runtime.evaluate result: %v", rawResultMap)
}
