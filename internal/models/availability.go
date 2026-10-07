package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const FreeModelCheckInterval = time.Hour
const FailedModelCheckInterval = 24 * time.Hour

type Availability struct {
	Model      string    `json:"model"`
	Disabled   bool      `json:"disabled"`
	CheckedAt  time.Time `json:"checked_at,omitempty"`
	NextCheck  time.Time `json:"next_check"`
	Reason     string    `json:"reason,omitempty"`
	Channel    string    `json:"channel,omitempty"`
	Generation uint64    `json:"-"`
}

type AvailabilityStore struct {
	mu    sync.RWMutex
	path  string
	items map[string]Availability
}

func NewAvailabilityStore(path string) (*AvailabilityStore, error) {
	s := &AvailabilityStore{path: path, items: map[string]Availability{}}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err = json.Unmarshal(b, &s.items); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if s.items == nil {
		s.items = map[string]Availability{}
	}
	return s, nil
}

func (s *AvailabilityStore) Get(model string) Availability {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item := s.items[model]
	item.Model = model
	return item
}

func (s *AvailabilityStore) Disabled(model string) bool { return s.Get(model).Disabled }

// A manual restore invalidates results from probes that were already running.
func (s *AvailabilityStore) Restore(model string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[model]
	previous, existed := s.items[model]
	item.Model, item.Disabled, item.Reason = model, false, "manually_enabled"
	item.NextCheck = now.Add(FreeModelCheckInterval)
	item.Generation++
	s.items[model] = item
	if err := s.saveLocked(); err != nil {
		if existed {
			s.items[model] = previous
		} else {
			delete(s.items, model)
		}
		return err
	}
	return nil
}

func (s *AvailabilityStore) Record(model string, generation uint64, success bool, reason, channel string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[model]
	if item.Generation != generation {
		return nil
	}
	item.Model, item.Disabled = model, !success
	item.CheckedAt, item.Reason, item.Channel = now, reason, channel
	interval := FreeModelCheckInterval
	if !success {
		interval = FailedModelCheckInterval
	}
	item.NextCheck = now.Add(interval)
	s.items[model] = item
	return s.saveLocked()
}

func (s *AvailabilityStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".availability-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
