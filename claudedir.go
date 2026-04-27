package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// claudeDirFor resolves the effective CLAUDE_CONFIG_DIR for an agent.
// Orchestrators get their own dir. Workers inherit their nearest
// orchestrator ancestor's. Dispatchers (and workers with no orchestrator
// ancestor) return "" meaning "use the user's global ~/.claude".
func claudeDirFor(kind, id, parentID string) (string, error) {
	switch kind {
	case "orchestrator":
		return orchestratorClaudeDir(id)
	case "worker":
		orch, err := findOrchestratorAncestor(parentID)
		if err != nil || orch == "" {
			return "", err
		}
		return orchestratorClaudeDir(orch)
	}
	return "", nil
}

// orchestratorClaudeDir returns the path for an orchestrator's isolated
// .claude dir and ensures it exists.
func orchestratorClaudeDir(id string) (string, error) {
	base, err := rosterPath("ROSTER_DIR", "XDG_DATA_HOME", ".local/share", "claude")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// findOrchestratorAncestor walks up the parent chain from startID until
// it hits an orchestrator. Returns "" if no orchestrator is found.
func findOrchestratorAncestor(startID string) (string, error) {
	cur := startID
	for i := 0; i < 20 && cur != ""; i++ {
		a, err := loadAgent(cur)
		if err != nil {
			return "", err
		}
		if a.Kind == "orchestrator" {
			return a.ID, nil
		}
		cur = a.Parent
	}
	return "", nil
}

// ensureAmuxSession creates the amux (tmux) session if it doesn't
// already exist. Safe to call repeatedly.
func ensureAmuxSession(session string) error {
	if err := exec.Command("amux", "exists", session).Run(); err == nil {
		return nil
	}
	if out, err := exec.Command("amux", "new", session).CombinedOutput(); err != nil {
		return fmt.Errorf("amux new %s: %s", session, strings.TrimSpace(string(out)))
	}
	return nil
}

// setTmuxSessionEnv sets one env var on a tmux session. Subsequent
// `tmux new-window -t <session>` invocations inherit it. Env set via
// plain process inheritance does NOT reach tmux-spawned windows, so
// this is how we propagate CLAUDE_CONFIG_DIR.
func setTmuxSessionEnv(session, key, value string) error {
	cmd := exec.Command("tmux", "set-environment", "-t", session, key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux set-environment: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// prepareClaudeIsolation does the full dance for one agent: resolve
// effective claude dir, create the tmux session if needed, set
// CLAUDE_CONFIG_DIR on it, seed onboarding state, and PATH-prepend
// the security shim so per-orch keychain ops land on the user's
// canonical entry. Returns the dir that will be used (empty means
// no override was applied). Safe to call even when the agent
// doesn't need isolation.
func prepareClaudeIsolation(kind, id, parentID, session string) (string, error) {
	dir, err := claudeDirFor(kind, id, parentID)
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", nil
	}
	// Fail loud if the user hasn't logged in to Claude on this
	// machine yet — otherwise the orch would boot, then crash on
	// first API call. Better error: "log in first."
	if !userKeychainHasClaudeCreds() {
		return "", fmt.Errorf("no Claude credentials found in keychain; log in with `claude` once first")
	}
	if kind == "orchestrator" {
		if err := seedOrchClaudeDir(dir); err != nil {
			return "", fmt.Errorf("seed orch claude dir: %w", err)
		}
	}
	shim, err := installSecurityShim()
	if err != nil {
		return "", fmt.Errorf("install security shim: %w", err)
	}
	if err := ensureAmuxSession(session); err != nil {
		return "", err
	}
	if err := setTmuxSessionEnv(session, "CLAUDE_CONFIG_DIR", dir); err != nil {
		return "", err
	}
	shimDir := filepath.Dir(shim)
	pathVal := shimDir + ":" + os.Getenv("PATH")
	if err := setTmuxSessionEnv(session, "PATH", pathVal); err != nil {
		return "", err
	}
	return dir, nil
}
