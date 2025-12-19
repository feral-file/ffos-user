package helper_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/helper"
)

func TestTruncateBytes(t *testing.T) {
	tests := []struct {
		name      string
		data      []byte
		maxLength int
		expected  []byte
	}{
		{
			name:      "empty byte slice",
			data:      []byte{},
			maxLength: 10,
			expected:  []byte{},
		},
		{
			name:      "data shorter than maxLength",
			data:      []byte{1, 2, 3, 4, 5},
			maxLength: 10,
			expected:  []byte{1, 2, 3, 4, 5},
		},
		{
			name:      "data exactly equal to maxLength",
			data:      []byte{1, 2, 3, 4, 5},
			maxLength: 5,
			expected:  []byte{1, 2, 3, 4, 5},
		},
		{
			name:      "data longer than maxLength",
			data:      []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			maxLength: 5,
			expected:  []byte{1, 2, 3, 4, 5},
		},
		{
			name:      "maxLength is zero",
			data:      []byte{1, 2, 3},
			maxLength: 0,
			expected:  []byte{},
		},
		{
			name:      "maxLength is one",
			data:      []byte{1, 2, 3, 4, 5},
			maxLength: 1,
			expected:  []byte{1},
		},
		{
			name:      "truncate string bytes",
			data:      []byte("hello world"),
			maxLength: 5,
			expected:  []byte("hello"),
		},
		{
			name:      "nil byte slice",
			data:      nil,
			maxLength: 10,
			expected:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := helper.TruncateBytes(tt.data, tt.maxLength)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestTruncateBytesDoesNotModifyOriginal(t *testing.T) {
	original := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	originalCopy := make([]byte, len(original))
	copy(originalCopy, original)

	_ = helper.TruncateBytes(original, 5)

	// Verify the original slice is not modified
	assert.Equal(t, originalCopy, original)
}

func TestTruncateMap(t *testing.T) {
	tests := []struct {
		name        string
		data        map[string]interface{}
		maxLength   int
		expectError bool
		validate    func(t *testing.T, result []byte, err error)
	}{
		{
			name:        "empty map",
			data:        map[string]interface{}{},
			maxLength:   100,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.Equal(t, []byte("{}"), result)
			},
		},
		{
			name: "small map within maxLength",
			data: map[string]interface{}{
				"key": "value",
			},
			maxLength:   100,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, result)
				assert.Contains(t, string(result), "key")
				assert.Contains(t, string(result), "value")
			},
		},
		{
			name: "map larger than maxLength",
			data: map[string]interface{}{
				"key1": "this is a long value that will be truncated",
				"key2": "another long value",
				"key3": 12345,
			},
			maxLength:   20,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, result)
				assert.Equal(t, 20, len(result))
			},
		},
		{
			name: "map with various data types",
			data: map[string]interface{}{
				"string":  "hello",
				"number":  42,
				"float":   3.14,
				"boolean": true,
				"null":    nil,
			},
			maxLength:   200,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, result)
				// Verify JSON is valid and contains expected keys
				jsonStr := string(result)
				assert.Contains(t, jsonStr, "string")
				assert.Contains(t, jsonStr, "number")
			},
		},
		{
			name: "map with nested objects",
			data: map[string]interface{}{
				"nested": map[string]interface{}{
					"inner": "value",
					"deep": map[string]interface{}{
						"level": 3,
					},
				},
			},
			maxLength:   200,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, result)
				assert.Contains(t, string(result), "nested")
			},
		},
		{
			name: "map with array",
			data: map[string]interface{}{
				"array": []interface{}{1, 2, 3, 4, 5},
				"key":   "value",
			},
			maxLength:   200,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, result)
				assert.Contains(t, string(result), "array")
			},
		},
		{
			name: "maxLength is zero",
			data: map[string]interface{}{
				"key": "value",
			},
			maxLength:   0,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.Equal(t, []byte{}, result)
			},
		},
		{
			name: "maxLength is very small",
			data: map[string]interface{}{
				"key": "value",
			},
			maxLength:   2,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.Equal(t, 2, len(result))
			},
		},
		{
			name:        "nil map",
			data:        nil,
			maxLength:   100,
			expectError: false,
			validate: func(t *testing.T, result []byte, err error) {
				assert.NoError(t, err)
				assert.Equal(t, []byte("null"), result)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := helper.TruncateMap(tt.data, tt.maxLength)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				tt.validate(t, result, err)
			}
		})
	}
}

func TestTruncateMapExactLength(t *testing.T) {
	// Test case where JSON output is exactly at maxLength
	data := map[string]interface{}{
		"a": "b",
	}

	// First, get the actual JSON length
	result, err := helper.TruncateMap(data, 1000)
	assert.NoError(t, err)
	exactLength := len(result)

	// Now truncate at exactly that length
	result2, err := helper.TruncateMap(data, exactLength)
	assert.NoError(t, err)
	assert.Equal(t, exactLength, len(result2))
	assert.Equal(t, result, result2)
}

func TestTruncateMapWithUnmarshallableData(t *testing.T) {
	// Test with data that cannot be marshaled to JSON
	data := map[string]interface{}{
		"func": func() {}, // functions cannot be marshaled to JSON
	}

	result, err := helper.TruncateMap(data, 100)
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestTruncateMapPreservesJSONStructure(t *testing.T) {
	// Test that truncation doesn't corrupt the beginning of valid JSON
	data := map[string]interface{}{
		"id":   "12345",
		"name": "test user",
		"age":  30,
	}

	result, err := helper.TruncateMap(data, 15)
	assert.NoError(t, err)
	assert.Equal(t, 15, len(result))

	// The beginning should still be valid JSON start (either '{' or part of a key)
	assert.True(t, result[0] == '{' || result[0] == '"')
}
