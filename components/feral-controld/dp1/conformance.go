package dp1

import (
	"bytes"
	"encoding/json"
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

// maxAuditPointers bounds how many violation locations one audit line carries.
const maxAuditPointers = 10

// AuditDocument reports how a received DP-1 document departs from the spec
// and returns nothing the caller may act on.
//
// Report-only by design (owner constraint): the device must keep playing every
// document it plays today, so a malformed dpVersion, a major mismatch, or a
// schema violation (missing required members, empty items, wrong types) is
// logged and never refuses the document. Only JSON pointers are logged: the
// validator's messages can quote document values, which are untrusted and may
// be large. Run on the bytes as received, before any reshaping, so the report
// describes the sender's document and not controld's rewrite of it.
func AuditDocument(logger *zap.Logger, source string, raw []byte) {
	if logger == nil {
		return
	}
	auditDPVersion(logger, source, raw)
	if err := dp1sdk.PlaylistWithPlaylistsAndContentRatingExtensionsSchemaValidate(raw); err != nil {
		pointers := ValidationPointers(err)
		if len(pointers) > maxAuditPointers {
			pointers = pointers[:maxAuditPointers]
		}
		logger.Warn("DP-1 document does not conform to the schema; playing it anyway",
			zap.String("source", source), zap.Strings("pointers", pointers))
	}
}

// auditSlot lets one background audit run at a time. The schema validator
// holds dp1-go's process-wide validation lock for a whole-document pass, so
// running it inline would put that cost on the display path of the command
// that carries the document. Off the path, the residual cost is that a
// concurrent command's own schema check (the synchronous content-rating
// fragment check shares that lock) can wait for one audit pass, bounded by one
// document (<= MaxPlaylistBodyBytes). The audit is report-only, so skipping one
// when another is in flight costs a log line, never behavior.
var auditSlot = make(chan struct{}, 1)

// AuditDocumentAsync runs AuditDocument off the caller's path, skipping it if
// an audit is already running. raw must not be mutated afterwards (callers
// hold the received bytes read-only).
func AuditDocumentAsync(logger *zap.Logger, source string, raw []byte) {
	select {
	case auditSlot <- struct{}{}:
		go func() {
			defer func() { <-auditSlot }()
			AuditDocument(logger, source, raw)
		}()
	default:
		if logger != nil {
			logger.Debug("DP-1 audit skipped: another audit is running", zap.String("source", source))
		}
	}
}

// WaitForAudits blocks until no background audit is running. It is a
// shutdown/teardown seam: an audit writes through the logger it was handed, so
// a caller whose logger has a bounded lifetime (a test's zaptest logger, which
// panics or races once the test has returned) drains audits before releasing
// it. Taking the slot is enough because AuditDocumentAsync takes it
// synchronously, before the goroutine starts, so an audit launched before this
// call is either holding the slot or finished. It does not stop new audits;
// call it once the source of documents has gone quiet. Production never needs
// it: the process logger outlives every audit.
func WaitForAudits() {
	auditSlot <- struct{}{}
	<-auditSlot
}

func auditDPVersion(logger *zap.Logger, source string, raw []byte) {
	var head struct {
		DPVersion *string `json:"dpVersion"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || head.DPVersion == nil {
		logger.Warn("DP-1 document has no readable dpVersion", zap.String("source", source))
		return
	}
	v := *head.DPVersion
	// ParseDPVersion is lenient ("1.0" parses); the schema is not.
	parsed, err := dp1sdk.ParseDPVersion(v)
	if err != nil || !dpVersionPattern.MatchString(v) {
		// Report the JSON pointer only: a malformed value is sender-controlled
		// document content and must not reach device logs.
		logger.Warn("DP-1 document dpVersion is not a version", zap.String("source", source), zap.String("pointer", "/dpVersion"))
		return
	}
	if err := dp1sdk.WarnMajorMismatch(parsed, SupportedMajor); err != nil {
		logger.Warn("DP-1 document major version differs from this build; playing it anyway",
			zap.String("source", source), zap.Uint64("major", parsed.Major()), zap.Uint64("supportedMajor", SupportedMajor))
	}
}

// knownPlaylistKeys is the set of top-level member names the typed Playlist
// decodes, lower-cased: encoding/json folds case when matching, so a member
// that folds to one of these was consumed by a field and must not also be
// carried as an extra.
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
				keys[strings.ToLower(name)] = true
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
		lower := strings.ToLower(k)
		// satellites is decoded by its exact key only (see UnmarshalJSON), so a
		// case variant is not consumed by any field and is carried as an extra.
		if knownPlaylistKeys[lower] && (lower != "satellites" || k == "satellites") {
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
