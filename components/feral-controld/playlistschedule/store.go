package playlistschedule

import (
	"fmt"
	"path/filepath"

	"github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Store persists the full displayAt playlist across controld-only restarts.
// The player only keeps the filtered active set, so restart recovery cannot
// reconstruct future scheduled items from player status alone.
type Store interface {
	Load() (*dp1.Playlist, error)
	Save(*dp1.Playlist) error
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

func (s *fileStore) Load() (*dp1.Playlist, error) {
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
	var playlist dp1.Playlist
	if err := s.json.Unmarshal(data, &playlist); err != nil {
		return nil, fmt.Errorf("decode displayAt playlist cache: %w", err)
	}
	return &playlist, nil
}

func (s *fileStore) Save(playlist *dp1.Playlist) error {
	if playlist == nil {
		return s.Clear()
	}
	if err := s.os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create displayAt playlist cache dir: %w", err)
	}
	data, err := s.json.Marshal(playlist)
	if err != nil {
		return fmt.Errorf("encode displayAt playlist cache: %w", err)
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
