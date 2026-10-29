package playerresponse

import "github.com/google/uuid"

// IsCanonicalShowingKey rejects legacy source-bearing identities without
// rewriting valid opaque IDs. Notifications and direct replies share this rule.
func IsCanonicalShowingKey(key string) bool {
	parsed, err := uuid.Parse(key)
	return err == nil && parsed.String() == key
}

// SanitizeShowingKey removes unsafe composition identities from a decoded
// checkStatus reply in place. Keep all other fields, including unknown fields,
// intact: direct replies carry more data than typed lightweight notifications.
func SanitizeShowingKey(result interface{}) {
	reply, ok := result.(map[string]interface{})
	if !ok {
		return
	}
	stripShowingKey(reply)
	if message, ok := reply["message"].(map[string]interface{}); ok {
		stripShowingKey(message)
	}
}

func stripShowingKey(reply map[string]interface{}) {
	settings, ok := reply["deviceSettings"].(map[string]interface{})
	if !ok {
		return
	}
	key, _ := settings["showingKey"].(string)
	if !IsCanonicalShowingKey(key) {
		delete(settings, "showingKey")
	}
}
