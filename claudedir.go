package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// claudeDirFor resolves the effective CLAUDE_CONFIG_DIR for an agent.
//
//   orchestrator → its own dir (per-orch isolation)
//   worker       → its nearest orchestrator ancestor's dir
//   dispatcher   → its own dir, separate from the user's ~/.claude.
//                  This keeps the dispatcher off any plugins/skills the
//                  user has installed globally — the dispatcher's job
//                  is to route, nothing else.
//
// Anything else returns "" meaning "use ~/.claude".
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
	case "dispatcher":
		return orchestratorClaudeDir(id)
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
// already exist. Safe to call repeatedly. The cwd is set on the
// session's default window so claude (and any subprocess) starts
// in the right place — see agentSpaceDir for the per-kind layout.
func ensureAmuxSession(session, cwd string) error {
	if err := exec.Command("amux", "exists", session).Run(); err == nil {
		return nil
	}
	args := []string{"new"}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	args = append(args, session)
	if out, err := exec.Command("amux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("amux new %s: %s", session, strings.TrimSpace(string(out)))
	}
	return nil
}

// agentSpaceDir returns the working-directory anchor for an agent.
// Each orchestrator owns <data>/<id>/, workers inherit their orch's,
// and the dispatcher lives at <data>/. This is THE dimension we use
// to keep an orch's domain artifacts (CSVs, notes, scaffolds) from
// stomping on another orch's.
//
// <data> is ~/Library/Application Support/Director by default. Honors
// $DIRECTOR_HOME first, then $FLOW_HOME (legacy from before the
// Flow→Director rename) so users with old config keep working.
func agentSpaceDir(kind, id, parentID string) string {
	base := os.Getenv("DIRECTOR_HOME")
	if base == "" {
		base = os.Getenv("FLOW_HOME")
	}
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "Library", "Application Support", "Director")
	}
	switch kind {
	case "orchestrator":
		return filepath.Join(base, id)
	case "worker":
		orch, _ := findOrchestratorAncestor(parentID)
		if orch != "" {
			return filepath.Join(base, orch)
		}
		return base
	}
	return base // dispatcher and anything else
}

// setTmuxSessionEnv sets one env var on a tmux session. Subsequent
// `tmux new-window -t <session>` invocations inherit it. Env set via
// plain process inheritance does NOT reach tmux-spawned windows, so
// this is how we propagate CLAUDE_CONFIG_DIR.
// injectPluginCreds walks the orch's installed plugins, looks up
// each declared credential in the macOS keychain (under fleetview's
// per-agent service), and sets it as a tmux session env var so plugin
// scripts can read $KEY directly. The keychain layout matches what
// fleetview's `POST /api/agents/:id/credentials` writes:
//
//   service: fleetview-<agentID>
//   account: <plugin>@<marketplace>/<key>
//
// Plugin authors don't have to know about keychain — they just read
// $KEY at runtime. Same env var name as the user typed in the form,
// no transformation. Missing values are silently skipped (env stays
// unset, plugin's own fallback kicks in).
func injectPluginCreds(claudeDir, agentID, session string) error {
	ipPath := filepath.Join(claudeDir, "plugins", "installed_plugins.json")
	b, err := os.ReadFile(ipPath)
	if err != nil {
		return nil // no plugins installed yet
	}
	var ip struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &ip); err != nil {
		return err
	}
	for key, entries := range ip.Plugins {
		if len(entries) == 0 {
			continue
		}
		pluginName, marketplace := splitPluginKey(key)
		credKeys := readDeclaredCredKeys(entries[0].InstallPath)
		for _, ck := range credKeys {
			val, ok := keychainGet(agentID, pluginName, marketplace, ck)
			if !ok {
				continue
			}
			if err := setTmuxSessionEnv(session, ck, val); err != nil {
				return err
			}
		}
	}
	return nil
}

// readDeclaredCredKeys returns the set of credential keys a plugin
// declares. Reads .claude-plugin/config.json (preferred) or falls back
// to .claude-plugin/credentials.json.
func readDeclaredCredKeys(installPath string) []string {
	cfg := filepath.Join(installPath, ".claude-plugin", "config.json")
	if b, err := os.ReadFile(cfg); err == nil {
		var doc struct {
			Credentials []struct {
				Key string `json:"key"`
			} `json:"credentials"`
		}
		if err := json.Unmarshal(b, &doc); err == nil {
			out := make([]string, 0, len(doc.Credentials))
			for _, c := range doc.Credentials {
				if c.Key != "" {
					out = append(out, c.Key)
				}
			}
			return out
		}
	}
	legacy := filepath.Join(installPath, ".claude-plugin", "credentials.json")
	if b, err := os.ReadFile(legacy); err == nil {
		var arr []struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(b, &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, c := range arr {
				if c.Key != "" {
					out = append(out, c.Key)
				}
			}
			return out
		}
	}
	return nil
}

// splitPluginKey parses "name@marketplace" — mirror of fleetview's.
func splitPluginKey(key string) (string, string) {
	i := strings.LastIndex(key, "@")
	if i <= 0 {
		return key, ""
	}
	return key[:i], key[i+1:]
}

// mirrorClaudeCredsToOrch copies the canonical "Claude Code-credentials"
// keychain entry into the per-orch suffixed entry that Claude Code
// looks up directly. claude-code computes the suffix as the first 8
// chars of sha256(CLAUDE_CONFIG_DIR), reading via Keychain Services
// (not the `security` CLI), which is why the existing PATH shim
// doesn't help.
//
// Idempotent: -U on add-generic-password updates in place. Skips
// silently if the canonical entry doesn't exist — the early check in
// prepareClaudeIsolation already errors loudly on that case.
func mirrorClaudeCredsToOrch(claudeDir string) error {
	canonical, err := runRealSecurity("find-generic-password",
		"-s", "Claude Code-credentials",
		"-a", os.Getenv("USER"),
		"-w")
	if err != nil {
		return nil
	}
	value := strings.TrimRight(string(canonical), "\n")
	if value == "" {
		return nil
	}
	suffix := claudeConfigDirHash(claudeDir)
	service := "Claude Code-credentials-" + suffix
	cmd := exec.Command("/usr/bin/security", "add-generic-password",
		"-s", service,
		"-a", os.Getenv("USER"),
		"-w", value,
		"-U")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("add-generic-password %s: %v — %s", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// claudeConfigDirHash mirrors how claude-code derives the keychain
// service suffix when CLAUDE_CONFIG_DIR is set: sha256[:8] of the dir.
func claudeConfigDirHash(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:])[:8]
}

// keychainGet reads one fleetview-managed credential from the user's
// macOS keychain. Returns (value, true) on hit, ("", false) on miss.
func keychainGet(agentID, pluginName, marketplace, key string) (string, bool) {
	// Use the *real* security binary, not the shim — the shim rewrites
	// to "Claude Code-credentials" which would clash with the per-agent
	// service we use for plugin creds.
	out, err := runRealSecurity("find-generic-password",
		"-s", "fleetview-"+agentID,
		"-a", fmt.Sprintf("%s@%s/%s", pluginName, marketplace, key),
		"-w")
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(out), "\n"), true
}

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
	switch kind {
	case "orchestrator":
		if err := seedOrchClaudeDir(dir); err != nil {
			return "", fmt.Errorf("seed orch claude dir: %w", err)
		}
	case "dispatcher":
		if err := seedDispatcherClaudeDir(dir); err != nil {
			return "", fmt.Errorf("seed dispatcher claude dir: %w", err)
		}
	}
	shim, err := installSecurityShim()
	if err != nil {
		return "", fmt.Errorf("install security shim: %w", err)
	}
	// Per-orch space: <data>/<orch-id>/ (workers inherit their parent's).
	// mkdir before creating the tmux session so amux's -c flag can land.
	space := agentSpaceDir(kind, id, parentID)
	if err := os.MkdirAll(space, 0o755); err != nil {
		return "", fmt.Errorf("mkdir agent space: %w", err)
	}
	if err := ensureAmuxSession(session, space); err != nil {
		return "", err
	}
	if err := setTmuxSessionEnv(session, "CLAUDE_CONFIG_DIR", dir); err != nil {
		return "", err
	}
	// Also set FLOW_SPACE on the session env so prompts and skills can
	// reference "$FLOW_SPACE" symbolically without hardcoding the path.
	if err := setTmuxSessionEnv(session, "FLOW_SPACE", space); err != nil {
		return "", err
	}
	shimDir := filepath.Dir(shim)
	pathVal := shimDir + ":" + os.Getenv("PATH")
	if err := setTmuxSessionEnv(session, "PATH", pathVal); err != nil {
		return "", err
	}
	// Inject any plugin-declared credentials into the session as env
	// vars. Best-effort — the agent still works without them; failures
	// here just mean a plugin script will hit the existing "$KEY not
	// set" path and tell the user to configure it.
	if err := injectPluginCreds(dir, id, session); err != nil {
		fmt.Fprintf(os.Stderr, "roster: plugin creds: %v\n", err)
	}
	// Mirror the user's canonical Claude OAuth tokens into the per-orch
	// suffixed keychain entry that Claude Code itself looks at. The
	// shell shim (PATH-prepended) can only redirect subprocess calls;
	// claude-code reads keychain directly via the macOS Keychain
	// Services API, which the shim doesn't intercept. Without this
	// mirror, every fresh orch boots into a "Not logged in" state even
	// though the user is signed in globally.
	if err := mirrorClaudeCredsToOrch(dir); err != nil {
		fmt.Fprintf(os.Stderr, "roster: mirror claude creds: %v\n", err)
	}
	return dir, nil
}
