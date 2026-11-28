package commandrouter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

func historyFailure(status, reason string) map[string]any {
	return map[string]any{"message": boundedFailureReply(map[string]any{"status": status, "error": reason})}
}

func (h *handler) readNativeHistory(ctx context.Context, method string, params, result any) map[string]any {
	if h.nativeControls == nil {
		return historyFailure("error", "Native player history is unavailable")
	}
	if err := h.nativeControls.Call(ctx, method, params, result, nil); err != nil {
		var remote *playerlink.RemoteError
		if errors.As(err, &remote) {
			status := "error"
			if remote.Code == "unsupported" || remote.Code == "unknown_method" {
				status = "unsupported"
			}
			return historyFailure(status, remote.Message)
		}
		h.logger.Warn("Native history read failed", zap.Error(err))
		return historyFailure("error", "Native player history is unavailable")
	}
	return nil
}

func validHistoryRecord(record playerlink.HistoryRecord) bool {
	source, _ := record.Item["source"].(string)
	return record.RecordID != "" && len(record.RecordID) <= 64 && record.PlayedAtMS != nil && *record.PlayedAtMS >= 0 && source != ""
}

func (h *handler) listNativeHistory(ctx context.Context) map[string]any {
	var history playerlink.History
	if failure := h.readNativeHistory(ctx, "history.list", map[string]any{}, &history); failure != nil {
		return failure
	}
	if history.Records == nil || history.Incomplete == nil || len(history.Records) > 100 {
		return historyFailure("error", "Native player returned invalid history")
	}
	var active *string
	if err := json.Unmarshal(history.ActiveOccurrence, &active); err != nil || (active != nil && (*active == "" || len(*active) > 64)) {
		return historyFailure("error", "Native player returned invalid history occurrence")
	}
	rows := make([]any, 0, len(history.Records))
	seen := make(map[string]bool)
	for _, record := range history.Records {
		if !validHistoryRecord(record) || seen[record.RecordID] {
			return historyFailure("error", "Native player returned invalid history")
		}
		seen[record.RecordID] = true
		metadata, _ := record.Item["metadata"].(map[string]any)
		if manifest, ok := record.Item["inlineManifest"].(map[string]any); ok && manifest["metadata"] != nil {
			metadata, _ = manifest["metadata"].(map[string]any)
		}
		title, _ := metadata["title"].(string)
		if title == "" {
			title, _ = record.Item["title"].(string)
		}
		var names []string
		artists, _ := metadata["artists"].([]any)
		for _, artist := range artists {
			person, _ := artist.(map[string]any)
			if name, ok := person["name"].(string); ok && name != "" {
				names = append(names, name)
			}
		}
		thumbnail := ""
		thumbnails, _ := metadata["thumbnails"].(map[string]any)
		for _, size := range []string{"small", "default", "large", "xlarge"} {
			image, _ := thumbnails[size].(map[string]any)
			if uri, ok := image["uri"].(string); ok && uri != "" {
				thumbnail = uri
				break
			}
		}
		rows = append(rows, map[string]any{
			"recordId": record.RecordID, "playedAtMs": float64(*record.PlayedAtMS),
			"isActive": active != nil && *active == record.RecordID,
			"itemId":   record.Item["id"], "title": title, "artist": strings.Join(names, ", "), "thumbnailUrl": thumbnail,
		})
	}
	status := "ok"
	if len(rows) == 0 {
		status = "empty"
	}
	// Preserve the external allowlist and byte limits, including URL sanitation.
	// The external flag means a retained row is currently displayed. Phones
	// keep polling while false, including restart and a failed history write.
	return boundedRecentlyPlayedReply(map[string]any{"message": map[string]any{
		"ok": true, "status": status, "activeOccurrenceKnown": active != nil && seen[*active],
		"incomplete": *history.Incomplete, "records": rows,
	}})
}

func (h *handler) resolveNativeHistory(ctx context.Context, id string) (playerlink.HistoryRecord, map[string]any) {
	var record playerlink.HistoryRecord
	if failure := h.readNativeHistory(ctx, "history.resolve", map[string]any{"recordId": id}, &record); failure != nil {
		return record, failure
	}
	if !validHistoryRecord(record) || record.RecordID != id {
		return record, historyFailure("error", "Native player returned an invalid history record")
	}
	return record, nil
}
