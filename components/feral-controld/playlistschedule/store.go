package playlistschedule

import (
	"fmt"
	"path/filepath"

	"github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Store persists the refreshable source identity for the active displayAt cast
// across controld-only restarts. The full playlist is intentionally not durable:
// restart recovery refetches the source, and if that fails the player keeps its
// current artwork instead of controld force-casting stale cached content.
type Store interface {
	Load() (*Source, error)
	Save(Source) error
	Clear() error
}

type fileStore struct {
	path string
	os   wrapper.OS
	json wrapper.JSON
}

func NewFileStore(os wrapper.OS, json wrapper.JSON) Store {
	return &fileStore{
		path: constant.DISPLAY_AT_PLAYLIST_FILE,
		os:   os,
		json: json,
	}
}

func (s *fileStore) Load() (*Source, error) {
	data, err := s.os.ReadFile(s.path)
	if s.os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read displayAt playlist cache: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var source Source
	if err := s.json.Unmarshal(data, &source); err != nil {
		return nil, fmt.Errorf("decode displayAt source cache: %w", err)
	}
	if source.IsZero() {
		return nil, nil
	}
	return &source, nil
}

func (s *fileStore) Save(source Source) error {
	if source.IsZero() {
		return s.Clear()
	}
	if err := s.os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create displayAt playlist cache dir: %w", err)
	}
	data, err := s.json.Marshal(source)
	if err != nil {
		return fmt.Errorf("encode displayAt source cache: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := s.os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write displayAt playlist cache: %w", err)
	}
	if err := s.os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("finalize displayAt playlist cache: %w", err)
	}
	return nil
}

func (s *fileStore) Clear() error {
	if err := s.os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create displayAt playlist cache dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := s.os.WriteFile(tmp, nil, 0o600); err != nil {
		return fmt.Errorf("clear displayAt playlist cache: %w", err)
	}
	if err := s.os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("finalize displayAt playlist cache clear: %w", err)
	}
	return nil
}
