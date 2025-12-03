//nolint:gosec
package wrapper

import (
	go_json "encoding/json"
	go_io "io"
)

//go:generate mockgen -source=json.go -destination=../mocks/json.go -package=mocks -mock_names=JSON=MockJSON
type JSON interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
	NewDecoder(r go_io.Reader) JSONDecoder
	NewEncoder(w go_io.Writer) JSONEncoder
}

type json struct{}

func NewJSON() JSON {
	return json{}
}

func (j json) Marshal(v any) ([]byte, error) {
	return go_json.Marshal(v)
}

func (j json) Unmarshal(data []byte, v any) error {
	return go_json.Unmarshal(data, v)
}

func (j json) NewDecoder(r go_io.Reader) JSONDecoder {
	return jsonDecoder{decoder: go_json.NewDecoder(r)}
}

func (j json) NewEncoder(w go_io.Writer) JSONEncoder {
	return jsonEncoder{encoder: go_json.NewEncoder(w)}
}

//go:generate mockgen -source=json.go -destination=../mocks/json.go -package=mocks -mock_names=JSONEncoder=MockJSONEncoder
type JSONEncoder interface {
	Encode(v any) error
}

type jsonEncoder struct {
	encoder *go_json.Encoder
}

func NewJSONEncoder() JSONEncoder {
	return jsonEncoder{}
}

func (j jsonEncoder) Encode(v any) error {
	return j.encoder.Encode(v)
}

//go:generate mockgen -source=json.go -destination=../mocks/json.go -package=mocks -mock_names=JSONDecoder=MockJSONDecoder
type JSONDecoder interface {
	Decode(v any) error
}

type jsonDecoder struct {
	decoder *go_json.Decoder
}

func NewJSONDecoder() JSONDecoder {
	return jsonDecoder{}
}

func (j jsonDecoder) Decode(v any) error {
	return j.decoder.Decode(v)
}
