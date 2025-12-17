package helper

import "encoding/json"

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
