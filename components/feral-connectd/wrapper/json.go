//nolint:gosec
package wrapper

import go_json "encoding/json"

//go:generate mockgen -source=json.go -destination=../mocks/json.go -package=mocks -mock_names=JSON=MockJSON
type JSON interface {
	Marshal(v interface{}) ([]byte, error)
	Unmarshal(data []byte, v interface{}) error
}

type json struct{}

func NewJSON() JSON {
	return json{}
}

func (j json) Marshal(v interface{}) ([]byte, error) {
	return go_json.Marshal(v)
}

func (j json) Unmarshal(data []byte, v interface{}) error {
	return go_json.Unmarshal(data, v)
}
