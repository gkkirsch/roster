package main

import (
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
// <data> is ~/Library/Application Support/Director by default; honors
// $DIRECTOR_HOME for users who want to relocate it.
func agentSpaceDir(kind, id, parentID string) string {
	base := os.Getenv("DIRECTOR_HOME")
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

// directorOAuthTokenService is the macOS keychain service name where
// Director stores its single long-lived OAuth token (created by the
// user via `claude setup-token`). The token is shared across every
// orch via the CLAUDE_CODE_OAUTH_TOKEN env var.
const directorOAuthTokenService = "Director-OAuth-Token"

// directorOAuthToken returns the user's Director-managed setup-token
// from keychain, or "" if none has been configured. Errors only on
// system-level failures (security CLI missing, keychain locked); a
// missing entry is not an error.
func directorOAuthToken() (string, error) {
	cmd := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", directorOAuthTokenService,
		"-a", os.Getenv("USER"),
		"-w")
	out, err := cmd.Output()
	if err != nil {
		// security exits 44 when the entry simply doesn't exist —
		// not an error from our perspective.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 44 {
			return "", nil
		}
		return "", fmt.Errorf("read keychain %s: %w", directorOAuthTokenService, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// keychainGet reads one fleetview-managed credential from the user's
// macOS keychain. Returns (value, true) on hit, ("", false) on miss.
func keychainGet(agentID, pluginName, marketplace, key string) (string, bool) {
	cmd := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", "fleetview-"+agentID,
		"-a", fmt.Sprintf("%s@%s/%s", pluginName, marketplace, key),
		"-w")
	out, err := cmd.Output()
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
// effective claude dir, create the tmux session, set CLAUDE_CONFIG_DIR
// + CLAUDE_CODE_OAUTH_TOKEN on it, seed onboarding state, and inject
// plugin creds. Returns the dir that will be used (empty means no
// override was applied).
//
// Auth model: every orch reads from the same Director-managed
// long-lived OAuth token (set up once via `claude setup-token` and
// stored in keychain). We propagate it as the CLAUDE_CODE_OAUTH_TOKEN
// env var on the tmux session, which claude-code reads directly —
// no per-orch keychain entry, no rotating-refresh-token race. The
// per-orch `Claude Code-credentials-<hash>` keychain entries that
// earlier versions of Director maintained are no longer used; they're
// left in place as orphans (harmless, removed on next OS reauth).
func prepareClaudeIsolation(kind, id, parentID, session string) (string, error) {
	dir, err := claudeDirFor(kind, id, parentID)
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", nil
	}
	// Fail loud if Director's OAuth token isn't configured yet — the
	// orch would otherwise boot and crash on first API call. The
	// setup page surfaces this and walks the user through
	// `claude setup-token`.
	token, err := directorOAuthToken()
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("Director auth token not configured; run `claude setup-token` and paste the result into the Director setup page")
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
	if err := setTmuxSessionEnv(session, "CLAUDE_CODE_OAUTH_TOKEN", token); err != nil {
		return "", err
	}
	// Also set DIRECTOR_SPACE on the session env so prompts and skills
	// can reference "$DIRECTOR_SPACE" symbolically without hardcoding
	// the path.
	if err := setTmuxSessionEnv(session, "DIRECTOR_SPACE", space); err != nil {
		return "", err
	}
	// Inject any plugin-declared credentials into the session as env
	// vars. Best-effort — the agent still works without them; failures
	// here just mean a plugin script will hit the existing "$KEY not
	// set" path and tell the user to configure it.
	if err := injectPluginCreds(dir, id, session); err != nil {
		fmt.Fprintf(os.Stderr, "roster: plugin creds: %v\n", err)
	}
	return dir, nil
}
