package dp1

import (
	"encoding/json"
	"regexp"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TEMPORARY (owner decision, 2026-11-26): remove this file, and call
// CheckPlayable instead of CheckInlineCast, once ff-app's wire converter is
// fixed, shipped, and made the app's required_version.
//
// Every released ff-app (1.9.13-1.9.35) builds inline casts that fail the
// schema in these ways, so refusing them would stop most casting from the app:
//   - the playlist or an item carries an app-internal `id` that is not a UUID
//     ("favorite", "addr:…", "source:…", a CAIP asset id);
//   - `created` is local time with no offset;
//   - the playlist `title`, which the app does not limit, is over 200
//     characters. Item titles have no limit in the schema;
//   - an address playlist is `items: []` with the legacy `dynamicQueries`,
//     where the schema allows an empty list only beside `dynamicQuery`.
//
// Nothing else is tolerated, and documents fetched from a feed or catalog get
// none of this.

var offsetlessDateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?$`)

// CheckInlineCast is CheckPlayable for a document a controller sent inline (and
// what controld keeps or rebuilds from one), with the allowances above.
func CheckInlineCast(logger *zap.Logger, source string, raw []byte) error {
	// Before relaxFFAppShapes, whose re-encode would drop a repeat.
	if err := refuseRepeatedMembers(logger, source, raw); err != nil {
		return err
	}
	relaxed, used := relaxFFAppShapes(raw)
	if err := CheckPlayable(logger, source, relaxed); err != nil {
		return err
	}
	if used && logger != nil {
		logger.Info("DP-1 inline document accepted under the temporary ff-app exemption", zap.String("source", source))
	}
	return nil
}

// relaxFFAppShapes returns a copy of the document, for checking only, with the
// tolerated departures taken out. The document played is the one received.
func relaxFFAppShapes(raw []byte) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return raw, false
	}
	used := dropNonUUIDID(doc)

	var title string
	if json.Unmarshal(doc["title"], &title) == nil && utf8.RuneCountInString(title) > MaxPlaylistTitle {
		doc["title"], _ = json.Marshal(string([]rune(title)[:MaxPlaylistTitle]))
		used = true
	}

	var created string
	if json.Unmarshal(doc["created"], &created) == nil && offsetlessDateTime.MatchString(created) {
		doc["created"], _ = json.Marshal(created + "Z")
		used = true
	}

	var items []json.RawMessage
	if json.Unmarshal(doc["items"], &items) == nil {
		changed := false
		for i := range items {
			var item map[string]json.RawMessage
			if json.Unmarshal(items[i], &item) != nil || !dropNonUUIDID(item) {
				continue
			}
			if out, err := marshalNoEscape(item); err == nil {
				items[i], changed = out, true
			}
		}
		if changed {
			if out, err := marshalNoEscape(items); err == nil {
				doc["items"], used = out, true
			}
		}
	}

	if isEmptyLegacyDynamic(doc) {
		doc["items"], used = placeholderItems, true
	}

	if !used {
		return raw, false
	}
	out, err := marshalNoEscape(doc)
	if err != nil {
		return raw, false
	}
	return out, true
}

// placeholderItems stands in, for the check only, for the empty list of a
// legacy dynamicQueries playlist.
var placeholderItems = json.RawMessage(`[{"source":"https://example.com/"}]`)

func isEmptyLegacyDynamic(doc map[string]json.RawMessage) bool {
	if _, spec := doc["dynamicQuery"]; spec {
		return false
	}
	var queries, items []json.RawMessage
	if json.Unmarshal(doc["dynamicQueries"], &queries) != nil || len(queries) == 0 {
		return false
	}
	return json.Unmarshal(doc["items"], &items) == nil && items != nil && len(items) == 0
}

// dropNonUUIDID removes an `id` that is a string but not a UUID. An id of
// another type stays and is refused.
func dropNonUUIDID(object map[string]json.RawMessage) bool {
	var id string
	raw, ok := object["id"]
	if !ok || json.Unmarshal(raw, &id) != nil || string(raw) == "null" {
		return false
	}
	// Canonical 36-character form only: uuid.Validate also takes urn: and
	// braced forms, which the schema does not.
	if len(id) == 36 && uuid.Validate(id) == nil {
		return false
	}
	delete(object, "id")
	return true
}
