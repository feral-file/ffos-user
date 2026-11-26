package dp1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	dp1sdk "github.com/display-protocol/dp1-go"
	"go.uber.org/zap"
)

// SupportedMajor is the DP-1 major version this build implements (§12).
const SupportedMajor = 1

// EmittedDPVersion is the dpVersion controld stamps on documents it derives
// itself (replay of one recently-played item). It must match the core schema
// pattern ^\d+\.\d+\.\d+$; "1.0" does not.
const EmittedDPVersion = "1.1.0"

var dpVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// maxSchemaPointers bounds how many violation locations one refusal names.
const maxSchemaPointers = 10

// SchemaError is the refusal of a document that does not conform to the DP-1
// schema. It carries JSON pointers only: the validator's messages can quote
// document values, which are untrusted and may be large.
type SchemaError struct{ Locations []string }

func (e *SchemaError) Error() string {
	return fmt.Sprintf("%v: schema at %s", ErrPlaylistInvalid, strings.Join(e.Locations, ", "))
}

// Unwrap carries the refusal to the transports as playlistInvalid.
func (e *SchemaError) Unwrap() error { return ErrPlaylistInvalid }

// placeholderSignatures stands in for whatever the document carries while the
// schema runs. A signature is not required of a playlist this device plays,
// and a bad one never blocks playback: whether signatures are checked, and
// what the verdict means, belongs to sigverify and its settings.
var placeholderSignatures = json.RawMessage(`[{"alg":"ed25519","kid":"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK",` +
	`"ts":"2025-01-01T00:00:00Z","payload_hash":"sha256:0000000000000000000000000000000000000000000000000000000000000000",` +
	`"role":"feed","sig":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]`)

// placeholderItems stands in for the empty list of a legacy dynamicQueries
// playlist, see CheckPlayable.
var placeholderItems = json.RawMessage(`[{"source":"https://example.com/"}]`)

// CheckPlayable refuses a playlist document that does not conform to the DP-1
// schema (core with the playlists and content-rating extensions). Every path
// that plays a document calls it, on the bytes as received or as stored,
// before anything reshapes them.
//
// Two allowances, both decided by the owner:
//   - signatures are not judged (see placeholderSignatures);
//   - a legacy `dynamicQueries` playlist may carry an empty `items`, which the
//     schema allows only next to the spec's `dynamicQuery`.
//
// A dpVersion of another major is only logged (§12). The validator holds
// dp1-go's process-wide lock for the whole pass, so concurrent commands queue
// behind a large document.
func CheckPlayable(logger *zap.Logger, source string, raw []byte) error {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return refuse(logger, source, []string{"/"})
	}
	if logger != nil {
		logMajorMismatch(logger, source, doc)
	}
	delete(doc, "signature")
	doc["signatures"] = placeholderSignatures
	if isEmptyLegacyDynamic(doc) {
		doc["items"] = placeholderItems
	}
	checked, err := marshalNoEscape(doc)
	if err != nil {
		return refuse(logger, source, []string{"/"})
	}
	if err := dp1sdk.PlaylistWithPlaylistsAndContentRatingExtensionsSchemaValidate(checked); err != nil {
		pointers := ValidationPointers(err)
		if len(pointers) > maxSchemaPointers {
			pointers = pointers[:maxSchemaPointers]
		}
		return refuse(logger, source, pointers)
	}
	return nil
}

// checkItem reports whether one item conforms, for items controld takes from a
// catalog or an indexer rather than from a playlist document.
func checkItem(raw []byte) error {
	doc, err := marshalNoEscape(map[string]any{
		"dpVersion":  EmittedDPVersion,
		"title":      "item",
		"items":      []json.RawMessage{raw},
		"signatures": placeholderSignatures,
	})
	if err != nil {
		return err
	}
	if err := dp1sdk.PlaylistWithPlaylistsAndContentRatingExtensionsSchemaValidate(doc); err != nil {
		return &SchemaError{Locations: ValidationPointers(err)}
	}
	return nil
}

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

func refuse(logger *zap.Logger, source string, pointers []string) error {
	if logger != nil {
		logger.Warn("DP-1 document does not conform to the schema; refusing it",
			zap.String("source", source), zap.Strings("pointers", pointers))
	}
	return &SchemaError{Locations: pointers}
}

// logMajorMismatch warns when the document's major differs from this build's.
// A malformed or missing dpVersion is the schema's to refuse.
func logMajorMismatch(logger *zap.Logger, source string, doc map[string]json.RawMessage) {
	var v string
	if json.Unmarshal(doc["dpVersion"], &v) != nil || !dpVersionPattern.MatchString(v) {
		return
	}
	parsed, err := dp1sdk.ParseDPVersion(v)
	if err != nil {
		return
	}
	if err := dp1sdk.WarnMajorMismatch(parsed, SupportedMajor); err != nil {
		// The parsed major, never the sender-sized string.
		logger.Warn("DP-1 document major version differs from this build; playing it anyway",
			zap.String("source", source), zap.Uint64("major", parsed.Major()), zap.Uint64("supportedMajor", SupportedMajor))
	}
}

// knownPlaylistKeys is the set of top-level member names the typed Playlist
// decodes, by exact name (stripCaseVariants keeps a case variant out of the
// decode), plus the reserved internal names in lower case.
var knownPlaylistKeys = func() map[string]bool {
	keys := map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				// Reserved internal field (Verification, Extra): its wire
				// name is claimed so a sender cannot smuggle content under it
				// through the extras.
				name = strings.ToLower(f.Name)
			}
			if name != "" {
				keys[name] = true
			}
		}
	}
	walk(reflect.TypeOf(Playlist{}))
	return keys
}()

// unknownPlaylistMembers returns the top-level members of the document the
// typed Playlist has no field for — what a later DP-1 minor, or an extension
// this build does not model, may add. Kept so the re-emit does not strip them
// (DP-1 §12 forward compatibility).
func unknownPlaylistMembers(data []byte) map[string]json.RawMessage {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil
	}
	var extra map[string]json.RawMessage
	for k, v := range all {
		// A case variant of a modeled member is consumed by no field, so it is
		// an unknown member like any other. The reserved names stay claimed
		// under any case.
		if knownPlaylistKeys[k] || strings.EqualFold(k, "verification") || strings.EqualFold(k, "extra") {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[k] = v
	}
	return extra
}

// marshalWithExtra encodes the typed playlist and splices the preserved
// unknown members back in. A typed field always wins over an extra of the same
// name (extras exclude every modeled name, so this only guards a future field).
func marshalWithExtra(base playlistJSON, extra map[string]json.RawMessage) ([]byte, error) {
	typed, err := marshalNoEscape(base)
	if err != nil || len(extra) == 0 {
		return typed, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(typed, &members); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, taken := members[k]; !taken {
			members[k] = v
		}
	}
	return marshalNoEscape(members)
}

// marshalNoEscape encodes without HTML escaping so the enclosing encoder's own
// SetEscapeHTML choice applies: bytes returned from a MarshalJSON are only
// re-escaped by the outer encoder when it escapes, and can never be
// un-escaped by one that does not.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
