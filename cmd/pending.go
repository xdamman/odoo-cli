package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PendingChange is one queued local mutation waiting to be pushed
// to Odoo. Stored as a JSON file under
// ~/.odoo/cache/<dbname>/pending/<id>.json.
//
// On successful push, the file is MOVED (not deleted) to sent/ so
// the audit trail survives. On failure, LastError gets stamped and
// the file stays in pending/ for the next run to retry.
type PendingChange struct {
	ID        string          `json:"id"`        // ULID-ish: nanosecond ts + short kind
	Kind      string          `json:"kind"`      // "reconcile", "categorize", "favorite-rename", …
	Payload   json.RawMessage `json:"payload"`   // kind-specific shape
	CreatedAt string          `json:"createdAt"` // RFC3339
	LastError string          `json:"lastError,omitempty"`
	Attempts  int             `json:"attempts,omitempty"`
}

// ReconcilePayload is the body of a "kind: reconcile" change.
type ReconcilePayload struct {
	JournalID       int `json:"journalId"`
	StatementLineID int `json:"statementLineId"`
	InvoiceMoveID   int `json:"invoiceMoveId"`
}

// AddPending writes a new pending-change file. Returns the change's
// ID so callers can reference it (e.g. for "queued change <id>" log).
func AddPending(dbname string, kind string, payload interface{}) (string, error) {
	if err := EnsureCacheDirs(dbname); err != nil {
		return "", err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%d-%s", time.Now().UnixNano(), sanitizeKindForID(kind))
	change := PendingChange{
		ID:        id,
		Kind:      kind,
		Payload:   body,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(change, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(PendingDir(dbname), id+".json")
	return id, os.WriteFile(path, data, 0600)
}

// ListPending returns every queued change for a database, sorted by
// createdAt ascending (FIFO).
func ListPending(dbname string) ([]PendingChange, error) {
	entries, err := os.ReadDir(PendingDir(dbname))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []PendingChange
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(PendingDir(dbname), e.Name()))
		if err != nil {
			continue
		}
		var c PendingChange
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// MovePendingToSent archives a successfully-pushed change.
func MovePendingToSent(dbname, id string) error {
	from := filepath.Join(PendingDir(dbname), id+".json")
	to := filepath.Join(SentDir(dbname), id+".json")
	if err := EnsureCacheDirs(dbname); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// StampPendingError records an error on a pending change without
// removing it from the queue. Increments Attempts.
func StampPendingError(dbname, id string, err error) error {
	path := filepath.Join(PendingDir(dbname), id+".json")
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return readErr
	}
	var c PendingChange
	if uerr := json.Unmarshal(data, &c); uerr != nil {
		return uerr
	}
	c.LastError = err.Error()
	c.Attempts++
	out, merr := json.MarshalIndent(c, "", "  ")
	if merr != nil {
		return merr
	}
	return os.WriteFile(path, out, 0600)
}

func sanitizeKindForID(kind string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(kind) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := b.String()
	if s == "" {
		return "change"
	}
	return s
}
