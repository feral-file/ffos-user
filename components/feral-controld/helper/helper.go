package helper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// TruncateBytes truncates the given bytes to the given max length
func TruncateBytes(data []byte, maxLength int) []byte {
	if len(data) <= maxLength {
		return data
	}
	return data[:maxLength]
}

// TruncateMap truncates the given map to the given max length
func TruncateMap(data map[string]interface{}, maxLength int) ([]byte, error) {
	// Convert map to JSON
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	// Truncate JSON data
	return TruncateBytes(jsonData, maxLength), nil
}

// TopicFingerprint is a short, non-reversible tag for a relayer topic ID, for
// logs. The topic is a bearer credential on the legacy relayer and logs are
// uploaded, so the raw value is never logged; the first 8 hex digits of its
// SHA-256 are enough to tell topics apart. Empty stays empty.
func TopicFingerprint(topic string) string {
	if topic == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(topic))
	return hex.EncodeToString(sum[:4])
}
