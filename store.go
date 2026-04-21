package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Agent is the file-backed record for one registered Claude session.
// Fields split into "durable" (written once at spawn, or on update) and
// "derived" (refreshed from camux/amux on list/describe).
type Agent struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`                    // dispatcher | orchestrator | worker
	Parent      string    `json:"parent,omitempty"`        // empty at top of tree
	Description string    `json:"description"`             // rolling summary, grows over time
	SessionUUID string    `json:"session_uuid,omitempty"`  // claude --resume handle
	SpawnArgs   []string  `json:"spawn_args"`              // camux spawn flags (post-<sess>)
	Cwd         string    `json:"cwd,omitempty"`
	Target      string    `json:"target,omitempty"`        // last known amux target
	Created     time.Time `json:"created"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
}

// KnownKinds enumerates values accepted for Kind. Descriptive, not enforced
// beyond a helpful error at spawn time.
var KnownKinds = []string{"dispatcher", "orchestrator", "worker"}

// validIDPattern keeps ids shell-friendly: letters, digits, dash, underscore.
var validIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func validateID(id string) error {
	if !validIDPattern.MatchString(id) {
		return fmt.Errorf("invalid id %q (allowed: letters, digits, '-', '_'; must not start with '-')", id)
	}
	return nil
}

func validateKind(kind string) error {
	for _, k := range KnownKinds {
		if k == kind {
			return nil
		}
	}
	return fmt.Errorf("invalid kind %q (want one of %s)", kind, strings.Join(KnownKinds, ", "))
}

// --- storage -----------------------------------------------------------------

// storeDir returns the per-user agent records directory, creating it if
// missing. Honors XDG_DATA_HOME, then $HOME/.local/share.
func storeDir() (string, error) {
	if d := os.Getenv("ROSTER_DIR"); d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
		return d, nil
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(base, "roster", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func agentPath(id string) (string, error) {
	dir, err := storeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

func loadAgent(id string) (*Agent, error) {
	p, err := agentPath(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no such agent %q", id)
		}
		return nil, err
	}
	var a Agent
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &a, nil
}

func saveAgent(a *Agent) error {
	p, err := agentPath(a.ID)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically: tmp file in same dir, rename.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func deleteAgent(id string) error {
	p, err := agentPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func agentExists(id string) bool {
	p, err := agentPath(id)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// listAgents returns all records, sorted by creation time ascending.
func listAgents() ([]*Agent, error) {
	dir, err := storeDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Agent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		a, err := loadAgent(id)
		if err != nil {
			// A corrupt file shouldn't kill the whole listing — warn and skip.
			fmt.Fprintf(os.Stderr, "roster: skipping %s: %v\n", e.Name(), err)
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created.Before(out[j].Created)
	})
	return out, nil
}

// childrenOf returns agents whose Parent == id, in creation order.
func childrenOf(id string, all []*Agent) []*Agent {
	var out []*Agent
	for _, a := range all {
		if a.Parent == id {
			out = append(out, a)
		}
	}
	return out
}
