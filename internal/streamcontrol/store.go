// Package streamcontrol owns Orion's durable stream intent, never scene content.
package streamcontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/ZabLaboratory/Orion/internal/canonical"
)

type Rule struct {
	Digest  string          `json:"digest"`
	Program json.RawMessage `json:"program"`
}
type App struct {
	Running bool `json:"running"`
	OnAir   bool `json:"on_air"`
}
type Intent struct {
	Rules map[string]Rule `json:"stream_rules"`
	Apps  map[string]App  `json:"overlay_apps"`
}
type document struct {
	LSML     string         `json:"lsml"`
	SceneID  string         `json:"scene_id"`
	Version  string         `json:"scene_version,omitempty"`
	Layout   map[string]any `json:"layout"`
	Defaults Intent         `json:"defaults"`
}
type Store struct {
	mu    sync.Mutex
	path  string
	value document
}

func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("STREAM_INTENT_ABSOLUTE_PATH_REQUIRED")
	}
	s := &Store{path: path, value: document{LSML: "1.1", SceneID: "orion-stream-control", Layout: map[string]any{"kind": "frame", "children": []any{}}, Defaults: Intent{Rules: map[string]Rule{}, Apps: map[string]App{}}}}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(raw, &s.value); err != nil {
			return nil, err
		}
		if s.value.LSML != "1.1" || s.value.SceneID != "orion-stream-control" || s.value.Defaults.Rules == nil || s.value.Defaults.Apps == nil {
			return nil, errors.New("STREAM_INTENT_INVALID")
		}
		version := s.value.Version
		if err := s.stamp(); err != nil {
			return nil, err
		}
		if version != s.value.Version {
			return nil, errors.New("STREAM_INTENT_DIGEST_MISMATCH")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else if err = s.save(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) Snapshot() Intent {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.value.Defaults)
	var result Intent
	_ = json.Unmarshal(raw, &result)
	return result
}
func (s *Store) change(change func(*Intent)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, _ := json.Marshal(s.value)
	change(&s.value.Defaults)
	if err := s.save(); err != nil {
		_ = json.Unmarshal(previous, &s.value)
		return err
	}
	return nil
}
func (s *Store) SetRule(id, digest string, program []byte) error {
	return s.change(func(i *Intent) { i.Rules[id] = Rule{digest, append(json.RawMessage(nil), program...)} })
}
func (s *Store) RemoveRule(id string) error { return s.change(func(i *Intent) { delete(i.Rules, id) }) }
func (s *Store) SetApp(id string, running, onAir *bool) error {
	return s.change(func(i *Intent) {
		app := i.Apps[id]
		if running != nil {
			app.Running = *running
		}
		if onAir != nil {
			app.OnAir = *onAir
		}
		i.Apps[id] = app
	})
}
func (s *Store) stamp() error {
	s.value.Version = ""
	raw, err := json.Marshal(s.value)
	if err != nil {
		return err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	s.value.Version, err = canonical.Digest(value)
	return err
}
func (s *Store) save() error {
	if err := s.stamp(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".stream-intent-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, s.path)
}
