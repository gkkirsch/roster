package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// AND symlinks it from ~/.local/bin/agent-browser so it lands on the
// user's normal PATH ahead of any global npm install. tmux
// set-environment doesn't reliably propagate PATH into new windows
// (default-shell zsh rebuilds PATH on startup), so the canonical
// install must already be somewhere zsh leaves on PATH.
//
// The wrapper falls through to the real agent-browser when
// AGENT_BROWSER_CDP is unset, so installing globally is safe — only
// roster orchs (where roster sets the env) get the enforced --cdp +
// blocked subcommands.
func installBrowserWrapper() (string, error) {
	canonical, err := installBrowserWrapperCanonical()
	if err != nil {
		return "", err
	}
	if err := ensureLocalBinSymlink("agent-browser", canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func installBrowserWrapperCanonical() (string, error) {
	dir, err := browserBinDir()
	if err != nil {
		return "", err
	}
	// Resolve the real agent-browser binary BEFORE we install our shim
	// — the discovery has to happen while the user's PATH still points
	// at the npm-installed (or other) original. Save it to a sidecar so
	// the wrapper can find it forever after.
	if err := writeRealAgentBrowserSidecar(dir); err != nil {
		// Non-fatal — the wrapper will surface a clear error if it
		// can't find the sidecar later.
		fmt.Fprintf(os.Stderr, "roster: agent-browser sidecar not written: %v\n", err)
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

// writeRealAgentBrowserSidecar resolves the real agent-browser binary
// path (skipping any symlink that already points at our shim) and
// writes it to <bin>/.agent-browser-real. The wrapper reads this at
// runtime to know where to exec — without it we'd risk recursing into
// our own symlink.
//
// Skipped if the sidecar already exists and points at an executable.
func writeRealAgentBrowserSidecar(binDir string) error {
	sidecar := filepath.Join(binDir, ".agent-browser-real")
	if existing, err := os.ReadFile(sidecar); err == nil {
		if fi, err := os.Stat(strings.TrimSpace(string(existing))); err == nil && fi.Mode()&0o111 != 0 {
			return nil
		}
	}
	ourShim := filepath.Join(binDir, "agent-browser")
	ourShimAbs, _ := filepath.Abs(ourShim)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || dir == binDir {
			continue
		}
		cand := filepath.Join(dir, "agent-browser")
		fi, err := os.Stat(cand)
		if err != nil || fi.Mode()&0o111 == 0 {
			continue
		}
		// Resolve through symlinks; skip if it ends up being our shim.
		resolved, err := filepath.EvalSymlinks(cand)
		if err != nil {
			continue
		}
		resolvedAbs, _ := filepath.Abs(resolved)
		if resolvedAbs == ourShimAbs {
			continue
		}
		return os.WriteFile(sidecar, []byte(resolved), 0o644)
	}
	return fmt.Errorf("no real agent-browser found on PATH (npm install -g agent-browser?)")
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
