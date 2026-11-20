package ff1config

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
)

type fakeFS map[string][]byte

func (f fakeFS) ReadFile(path string) ([]byte, error) {
	data, ok := f[path]
	if !ok {
		return nil, errors.New("not found")
	}
	return data, nil
}

func TestReadParsesTheDescriptor(t *testing.T) {
	fs := fakeFS{constants.FF1_CONFIG_FILE: []byte(`{"version":"3.0.1","branch":"release","endpoint":"https://example.test","extra":1}`)}
	b, err := Read(fs, stdJSON{})
	require.NoError(t, err)
	assert.Equal(t, Build{Version: "3.0.1", Branch: "release", Endpoint: "https://example.test"}, b)
}

func TestReadReportsMissingAndMalformed(t *testing.T) {
	_, err := Read(fakeFS{}, stdJSON{})
	require.ErrorContains(t, err, "reading FF1 config")

	_, err = Read(fakeFS{constants.FF1_CONFIG_FILE: []byte(`{`)}, stdJSON{})
	var syntax *json.SyntaxError
	require.ErrorAs(t, err, &syntax)
	assert.ErrorContains(t, err, "parsing FF1 config")
}
