package offlinecache

import "fmt"

// Automatic records use the same path as explicit snapshots, but a versioned
// envelope. Old binaries see no top-level item.source, fail their existing
// filename/source identity check and quarantine the record on startup. They
// can never turn a TTL into a permanent snapshot. An old explicit download
// overwrites this same path, and an old clear deletes it: no hidden fallback
// record can resurrect cleared artwork on a later upgrade.
type automaticItemEnvelope struct {
	Version int         `json:"automaticCacheVersion"`
	Record  *ItemRecord `json:"record"`
}

func (s *fsStore) decodeItem(data []byte, rec *ItemRecord) error {
	var envelope automaticItemEnvelope
	if err := s.json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Version == 0 {
		return s.json.Unmarshal(data, rec)
	}
	if envelope.Version != 1 || envelope.Record == nil || !envelope.Record.Automatic {
		return fmt.Errorf("unsupported or invalid automatic cache record version %d", envelope.Version)
	}
	*rec = *envelope.Record
	return nil
}
