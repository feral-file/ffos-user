package commandrouter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

type capsuleCastKey struct{}

func isCapsuleCast(ctx context.Context) bool {
	return ctx.Value(capsuleCastKey{}) == true
}

func (h *handler) handleDisplayCapsule(ctx context.Context, args map[string]any) (interface{}, error) {
	principal, ok := control.PrincipalFrom(ctx)
	if !ok || principal.Role != control.RoleOwner {
		return nil, ErrPermissionDenied
	}
	importer, ok := h.offlineCache.(interface {
		ImportCapsule(context.Context, []byte, json.RawMessage, string) (json.RawMessage, error)
	})
	if !ok {
		return nil, errors.New("capsule import requires the offline cache")
	}

	encoded, _ := args["capsule"].(string)
	signer, _ := args["signer"].(string)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(offlinecache.MaxCapsuleBytes) || signer == "" {
		return nil, errors.New("displayCapsule requires a bounded base64 capsule and signer DID")
	}
	archive, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, errors.New("invalid capsule base64")
	}
	signature, err := json.Marshal(args["signature"])
	if err != nil {
		return nil, errors.New("invalid capsule signature document")
	}
	raw, err := importer.ImportCapsule(ctx, archive, signature, signer)
	if err != nil {
		return nil, fmt.Errorf("import capsule: %w", err)
	}
	var playlist map[string]any
	if err := json.Unmarshal(raw, &playlist); err != nil {
		return nil, err
	}
	// Use the normal policy, scheduler and native player dispatch. Capsule
	// signature verification covered these bytes, and an origin probe cannot
	// tell us whether the installed capture can replay.
	rawArguments, err := json.Marshal(map[string]json.RawMessage{"dp1_call": raw})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(archive)
	return h.Process(context.WithValue(ctx, capsuleCastKey{}, true), commands.Command{
		Type:             commands.CMD_DISPLAY_PLAYLIST,
		Arguments:        map[string]any{"dp1_call": playlist},
		RawArguments:     rawArguments,
		OfflineCapsuleID: hex.EncodeToString(digest[:]),
	})
}
