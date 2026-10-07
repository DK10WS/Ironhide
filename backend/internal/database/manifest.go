package database

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

type ManifestEntry struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
	ETag string `json:"etag,omitempty"`
}

type Manifest []ManifestEntry

func (m Manifest) Value() (driver.Value, error) {
	if m == nil {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	return b, nil
}

func (m *Manifest) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case nil:
		*m = nil
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("cannot scan %T into Manifest", src)
	}

	if len(raw) == 0 {
		*m = Manifest{}
		return nil
	}

	var entries []ManifestEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("unmarshal manifest: %w", err)
	}
	*m = entries
	return nil
}

func (m Manifest) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("[]"), nil
	}
	type plain []ManifestEntry
	return json.Marshal(plain(m))
}

func (m *Manifest) UnmarshalJSON(b []byte) error {
	var entries []ManifestEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return err
	}
	*m = entries
	return nil
}

func (Manifest) GormDataType() string { return "jsonb" }

func (m Manifest) Total() int64 {
	var n int64
	for _, e := range m {
		n += e.Size
	}
	return n
}

func (m Manifest) Keys() []string {
	out := make([]string, 0, len(m))
	for _, e := range m {
		out = append(out, e.Key)
	}
	return out
}
