//nolint:gosec
package wrapper

import "encoding/json"

//go:generate mockgen -source=json.go -destination=../mocks/json.go -package=mocks -mock_names=JSONInterface=MockJSON
type JSONInterface interface {
	Marshal(v interface{}) ([]byte, error)
	Unmarshal(data []byte, v interface{}) error
}

type JSON struct{}

func NewJSON() JSONInterface {
	return JSON{}
}

func (j JSON) Marshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func (j JSON) Unmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
