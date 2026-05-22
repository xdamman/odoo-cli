package cmd

import (
	"encoding/json"
	"os"
	"time"
)

// State is the global, machine-wide CLI state. Lives at
// ~/.odoo/state.json and tracks the currently-active database plus
// recent-use timestamps for the `switch` picker.
type State struct {
	ActiveDB string            `json:"activeDb,omitempty"`
	LastUsed map[string]string `json:"lastUsed,omitempty"` // dbname → RFC3339 timestamp
}

// LoadState reads ~/.odoo/state.json. Returns a zero State (no
// error) when the file is missing — first-run is normal.
func LoadState() (*State, error) {
	data, err := os.ReadFile(StateFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return &State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return &State{}, err
	}
	if s.LastUsed == nil {
		s.LastUsed = map[string]string{}
	}
	return &s, nil
}

// SaveState persists ~/.odoo/state.json. Creates the app root if
// missing. Always 0600 to keep operational state private.
func SaveState(s *State) error {
	if s == nil {
		return nil
	}
	if err := EnsureAppRoot(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(StateFilePath(), data, 0600)
}

// SetActiveDB updates the persistent active database. Also stamps
// lastUsed for the picker's ordering.
func SetActiveDB(name string) error {
	s, _ := LoadState()
	s.ActiveDB = name
	if s.LastUsed == nil {
		s.LastUsed = map[string]string{}
	}
	s.LastUsed[name] = time.Now().UTC().Format(time.RFC3339)
	return SaveState(s)
}

// TouchActive bumps the lastUsed timestamp for the active DB
// without changing which one is active. Called at the start of
// every command that reads from a DB so the picker can show
// "recently used" first.
func TouchActive(name string) {
	if name == "" {
		return
	}
	s, _ := LoadState()
	if s.LastUsed == nil {
		s.LastUsed = map[string]string{}
	}
	s.LastUsed[name] = time.Now().UTC().Format(time.RFC3339)
	_ = SaveState(s)
}
