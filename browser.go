package main

import (
	"bytes"
	_ "embed"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
)

// Per-orchestrator browser isolation. One Chrome profile per orch, one
// CDP port per orch (deterministic from the orch ID), and a wrapper on
// PATH that forces every `agent-browser` invocation through that port.
// Workers inherit from their nearest orch ancestor — same shape as
// claudedir.go.

//go:embed assets/agent-browser
var agentBrowserWrapper []byte

const (
	// Range chosen to avoid Chrome's default 9222 and Vite/dev-server
	// ports. 100 slots → 100 simultaneous orchs before we collide.
	cdpPortBase  = 9300
	cdpPortRange = 100
)

// cdpPortFor returns the deterministic CDP port for an orch.
func cdpPortFor(orchID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(orchID))
	return cdpPortBase + int(h.Sum32()%uint32(cdpPortRange))
}

// browserProfileDir returns the per-orch Chrome user-data-dir, ensuring
// it exists.
func browserProfileDir(orchID string) (string, error) {
	base, err := rosterPath("ROSTER_DIR", "XDG_DATA_HOME", ".local/share", "browser-profiles")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, orchID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// browserBinDir is where the wrapper lives. Shared across orchs — the
// wrapper reads AGENT_BROWSER_CDP from env at run time.
func browserBinDir() (string, error) {
	return rosterPath("ROSTER_DIR", "XDG_DATA_HOME", ".local/share", "bin")
}

// installBrowserWrapper writes the embedded wrapper to <bin>/agent-browser
// if missing or out of date. Idempotent and cheap.
func installBrowserWrapper() (string, error) {
	dir, err := browserBinDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "agent-browser")
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, agentBrowserWrapper) {
		_ = os.Chmod(path, 0o755)
		return path, nil
	}
	if err := os.WriteFile(path, agentBrowserWrapper, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// browserOrchFor resolves which orch's browser context an agent should
// use. Orchestrators answer for themselves; workers walk up to their
// nearest orch; dispatchers (and orphan workers) get "" — meaning no
// browser env is provisioned.
func browserOrchFor(kind, id, parentID string) (string, error) {
	switch kind {
	case "orchestrator":
		return id, nil
	case "worker":
		return findOrchestratorAncestor(parentID)
	}
	return "", nil
}

// prepareBrowserIsolation does the per-spawn work: install the wrapper,
// compute the per-orch port + profile, and set env on the tmux session
// (CDP port, session name, profile dir, and a PATH that includes our
// wrapper bin dir). Safe to call for any agent — no-ops cleanly when
// the agent has no orch ancestor.
//
// Returns the resolved orch ID (empty if no isolation applied).
func prepareBrowserIsolation(kind, id, parentID, session string) (string, error) {
	orch, err := browserOrchFor(kind, id, parentID)
	if err != nil {
		return "", err
	}
	if orch == "" {
		return "", nil
	}
	bin, err := installBrowserWrapper()
	if err != nil {
		return "", err
	}
	profile, err := browserProfileDir(orch)
	if err != nil {
		return "", err
	}
	if err := ensureAmuxSession(session); err != nil {
		return "", err
	}
	port := cdpPortFor(orch)
	binDir := filepath.Dir(bin)
	pathVal := binDir + ":" + os.Getenv("PATH")
	env := map[string]string{
		"AGENT_BROWSER_CDP":     strconv.Itoa(port),
		"AGENT_BROWSER_SESSION": orch,
		"AGENT_BROWSER_PROFILE": profile,
		"PATH":                  pathVal,
	}
	for k, v := range env {
		if err := setTmuxSessionEnv(session, k, v); err != nil {
			return "", err
		}
	}
	return orch, nil
}
