// Package relaywire implements issue #25's bounded opaque transport framing.
// Routing identity is untrusted; callers must authenticate payloads before use.
package relaywire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"unicode/utf8"
)

const (
	MaxFrameBytes  = 65536
	MaxHeaderBytes = 1024
	Subprotocol    = "ff-relay.v2"
)

var routingID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var playerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var errFrame = errors.New("invalid relay frame")

// Header identifies a transport packet, not an authenticated command or grant.
type Header struct {
	Version int    `json:"v"`
	ID      string `json:"id"`
	To      string `json:"to"`
	From    string `json:"from,omitempty"`
}

func valid(h Header) bool {
	return h.Version == 2 && routingID.MatchString(h.ID) && routingID.MatchString(h.To) && (h.From == "" || routingID.MatchString(h.From))
}

// Encode copies opaque bytes into one bounded WebSocket binary message.
func Encode(h Header, payload []byte) ([]byte, error) {
	if !valid(h) || len(payload) > MaxFrameBytes {
		return nil, errFrame
	}
	header, err := json.Marshal(h)
	if err != nil {
		return nil, errFrame
	}
	if len(header) > MaxHeaderBytes || len(header)+len(payload)+2 > MaxFrameBytes {
		return nil, errFrame
	}
	frame := make([]byte, 2+len(header)+len(payload))
	binary.BigEndian.PutUint16(frame, uint16(len(header))) // #nosec G115 -- header is bounded to 1024 bytes above.
	copy(frame[2:], header)
	copy(frame[2+len(header):], payload)
	return frame, nil
}

// Decode returns a payload view. It neither parses nor authenticates that payload.
func Decode(frame []byte) (Header, []byte, error) {
	var h Header
	if len(frame) < 3 || len(frame) > MaxFrameBytes {
		return h, nil, errFrame
	}
	n := int(binary.BigEndian.Uint16(frame))
	if n == 0 || n > MaxHeaderBytes || n+2 > len(frame) {
		return h, nil, errFrame
	}
	header := frame[2 : 2+n]
	if !utf8.Valid(header) || json.Unmarshal(header, &h) != nil || !valid(h) {
		return Header{}, nil, errFrame
	}
	return h, frame[2+n:], nil
}

// ConnectionURL routes by public device ID without a topic or bearer credential.
func ConnectionURL(endpoint, deviceID string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !playerID.MatchString(deviceID) {
		return "", errors.New("invalid device relay endpoint")
	}
	u.Path = "/v2/channels/" + deviceID + "/connections"
	return u.String(), nil
}
