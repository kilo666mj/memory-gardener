// Package state persists what the gardener has asked and what was decided,
// so it never asks twice about the same findings.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Proposal is an open review task for one memory.
type Proposal struct {
	MemoryID     string    `json:"memory_id"`
	TaskID       string    `json:"task_id"`
	EscalationID string    `json:"escalation_id"`
	Verdict      string    `json:"verdict"`
	Replacement  string    `json:"replacement,omitempty"`
	Fingerprint  string    `json:"fingerprint"`
	CreatedAt    time.Time `json:"created_at"`
}

// Review records the latest decision about a memory's findings.
type Review struct {
	At          time.Time `json:"at"`
	Outcome     string    `json:"outcome"`
	Fingerprint string    `json:"fingerprint"`
}

// State is the gardener's durable memory.
type State struct {
	Proposals map[string]Proposal `json:"proposals"`
	Reviews   map[string]Review   `json:"reviews"`
}

// Load reads the state file; a missing file is an empty state.
func Load(file string) (*State, error) {
	s := &State{Proposals: map[string]Proposal{}, Reviews: map[string]Review{}}
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	if s.Proposals == nil {
		s.Proposals = map[string]Proposal{}
	}
	if s.Reviews == nil {
		s.Reviews = map[string]Review{}
	}
	return s, nil
}

// Save writes the state atomically.
func (s *State) Save(file string) (err error) {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".state-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
