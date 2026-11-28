// Package playerlink implements the versioned native-player Unix socket protocol.
// It carries typed operations and observations, never JavaScript or CDP methods.
package playerlink

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

const Version = 1
const MaxFrameBytes = 4 * 1024 * 1024

// RemoteError distinguishes a known refusal from an uncertain transport outcome.
type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RemoteError) Error() string { return e.Code + ": " + e.Message }

type Request struct {
	Version    int     `json:"v"`
	ID         string  `json:"id"`
	Method     string  `json:"method"`
	Params     any     `json:"params"`
	Instance   string  `json:"instance,omitempty"`
	Generation *uint64 `json:"generation,omitempty"`
	Revision   *uint64 `json:"ifRevision,omitempty"`
	DeadlineMS int64   `json:"deadlineMs,omitempty"`
	Key        string  `json:"key,omitempty"`
}

type Response struct {
	Version int             `json:"v"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RemoteError    `json:"error"`
}

// ValidateJSON shares the C++ parser's duplicate-member and depth policy. The
// standard Go decoder otherwise accepts the last duplicate value silently.
func ValidateJSON(body []byte) error {
	if len(body) == 0 || len(body) > MaxFrameBytes {
		return errors.New("frame_size")
	}
	if !utf8.Valid(body) {
		return errors.New("invalid_json: invalid UTF-8")
	}
	if err := validateSurrogates(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := validateValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("invalid_json: trailing data")
	}
	return nil
}

func validateValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("invalid_json: excessive nesting")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		if number, ok := token.(json.Number); ok {
			if _, err := strconv.ParseFloat(number.String(), 64); err != nil {
				return errors.New("invalid_json: number exceeds finite double range")
			}
		}
		return nil
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errors.New("invalid_json: duplicate member")
			}
			keys[name] = true
		}
		if err := validateValue(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// encoding/json silently replaces lone UTF-16 surrogates. C++ rejects them;
// inspect escapes before decoding so the two implementations accept the same
// scalar-value strings, without mistaking an escaped backslash for a surrogate.
func validateSurrogates(body []byte) error {
	inString := false
	for i := 0; i < len(body); i++ {
		if body[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || body[i] != '\\' {
			continue
		}
		if i+1 >= len(body) {
			return errors.New("invalid_json: truncated escape")
		}
		if body[i+1] != 'u' {
			i++
			continue
		}
		if i+6 > len(body) {
			return errors.New("invalid_json: truncated Unicode escape")
		}
		value, err := strconv.ParseUint(string(body[i+2:i+6]), 16, 16)
		if err != nil {
			return errors.New("invalid_json: invalid Unicode escape")
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+12 > len(body) || body[i+6] != '\\' || body[i+7] != 'u' {
				return errors.New("invalid_json: unpaired high surrogate")
			}
			low, err := strconv.ParseUint(string(body[i+8:i+12]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return errors.New("invalid_json: invalid surrogate pair")
			}
			i += 11
		} else {
			if value >= 0xdc00 && value <= 0xdfff {
				return errors.New("invalid_json: unpaired low surrogate")
			}
			i += 5
		}
	}
	return nil
}

func WriteFrame(writer io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > MaxFrameBytes {
		return errors.New("frame_size")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body))) //nolint:gosec // G115: length is bounded to 4 MiB above.
	for _, part := range [][]byte{prefix[:], body} {
		for len(part) > 0 {
			n, err := writer.Write(part)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}

func ReadFrame(reader io.Reader, target any) error {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size == 0 || size > MaxFrameBytes {
		return errors.New("frame_size")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return err
	}
	if err := ValidateJSON(body); err != nil {
		return fmt.Errorf("player response: %w", err)
	}
	return json.Unmarshal(body, target)
}
