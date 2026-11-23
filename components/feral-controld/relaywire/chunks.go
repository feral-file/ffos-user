package relaywire

import (
	"encoding/json"
	"errors"
	"time"
)

const ChunkBytes = 32768
const MaxMessageBytes = 4 << 20

// Chunk wraps transport fragments only; reassembly restores the exact JSON bytes.
type Chunk struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
	Count int    `json:"count"`
	Data  []byte `json:"data"`
}

type chunkEnvelope struct {
	Chunk *Chunk `json:"relayChunk"`
}

// Split bounds each transport payload while leaving ordinary JSON unchanged.
func Split(id string, body []byte) ([][]byte, error) {
	if len(body) > MaxMessageBytes {
		return nil, errors.New("relay message too large")
	}
	if len(body) <= ChunkBytes {
		return [][]byte{body}, nil
	}
	count := (len(body) + ChunkBytes - 1) / ChunkBytes
	parts := make([][]byte, 0, count)
	for offset := 0; offset < len(body); offset += ChunkBytes {
		end := min(offset+ChunkBytes, len(body))
		part, err := json.Marshal(chunkEnvelope{&Chunk{id, offset / ChunkBytes, count, body[offset:end]}})
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// Assembler retains at most one bounded, ordered message for each live peer.
// Callers remove peer state on disconnect; peers cannot extend a chunk deadline.
type Assembler struct {
	id      string
	count   int
	next    int
	body    []byte
	expires time.Time
	// replaced is set when a new transfer's first chunk discards one still
	// being assembled; TakeReplaced reads and clears it.
	replaced bool
}

// Partial reports a chunked message still being assembled: the chunks held
// and the chunks its sender announced.
func (a *Assembler) Partial() (held, announced int, ok bool) {
	return a.next, a.count, a.count > 0 && a.next < a.count
}

// TakeReplaced reports, once, that the last first chunk discarded a transfer
// still being assembled: its sender began again, or began something else,
// without finishing.
func (a *Assembler) TakeReplaced() bool {
	replaced := a.replaced
	a.replaced = false
	return replaced
}

// Add validates a fragment before retaining it and returns nil until complete.
func (a *Assembler) Add(body []byte, now time.Time) ([]byte, error) {
	var envelope chunkEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	c := envelope.Chunk
	if c == nil {
		return body, nil
	}
	if !routingID.MatchString(c.ID) || c.Count < 2 || c.Count > MaxMessageBytes/ChunkBytes || c.Index < 0 || c.Index >= c.Count || len(c.Data) == 0 || len(c.Data) > ChunkBytes {
		return nil, errors.New("invalid relay chunk")
	}
	if c.Index == 0 {
		_, _, partial := a.Partial()
		*a = Assembler{id: c.ID, count: c.Count, expires: now.Add(180 * time.Second), replaced: partial}
	}
	if a.id != c.ID || a.count != c.Count || a.next != c.Index || !now.Before(a.expires) || len(a.body)+len(c.Data) > MaxMessageBytes {
		*a = Assembler{}
		return nil, errors.New("out of order or expired relay chunk")
	}
	a.body = append(a.body, c.Data...)
	a.next++
	if a.next != a.count {
		return nil, nil
	}
	result := a.body
	*a = Assembler{replaced: a.replaced}
	return result, nil
}
