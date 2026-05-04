package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Per-orch CLAUDE_CONFIG_DIR isolation breaks first-launch's theme
// picker and onboarding flow — we seed .claude.json with theme +
// hasCompletedOnboarding so interactiveHelpers.tsx:111 short-circuits
// the Onboarding component.
//
// Auth is independent: each orch uses the user's Director OAuth token
// (set up once via `claude setup-token`, stored in keychain under
// `Director-OAuth-Token`) propagated as CLAUDE_CODE_OAUTH_TOKEN on
// every tmux session. claude-code reads the env var directly and
// never touches the keychain when it's set, so all orchs share one
// long-lived token with no rotation race.

//go:embed assets/skill-agent-browser.md
var skillAgentBrowser []byte

//go:embed assets/skill-artifact.md
var skillArtifact []byte

// ensureLocalBinSymlink creates ~/.local/bin/<name> → target. Idempotent;
// refuses to clobber an existing file that isn't already our symlink, so
// we never blow away a script the user wrote.
func ensureLocalBinSymlink(name, target string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(binDir, name)
	if existing, err := os.Readlink(link); err == nil {
		if existing == target {
			return nil
		}
		// Stale roster symlink (e.g. moved bin dir) → replace.
		if existing != target && filepath.Base(filepath.Dir(existing)) == "bin" && filepath.Base(filepath.Dir(filepath.Dir(existing))) == "roster" {
			_ = os.Remove(link)
		} else {
			return fmt.Errorf("%s exists and points elsewhere (%s); refusing to overwrite", link, existing)
		}
	} else if _, err := os.Lstat(link); err == nil {
		return fmt.Errorf("%s exists and isn't a symlink; refusing to overwrite", link)
	}
	return os.Symlink(target, link)
}

// seedOrchClaudeDir provisions a fresh per-orch CLAUDE_CONFIG_DIR so
// it skips all three first-launch dialogs (theme, login, bypass-perms
// consent) and ships built-in tooling skills. Idempotent: existing
// fields are preserved.
//
// Files written:
//
//   .claude.json                       theme, hasCompletedOnboarding,
//                                      lastOnboardingVersion, oauthAccount
//                                      (copied from user's ~/.claude)
//
//   settings.json                      skipDangerousModePermissionPrompt:true
//                                      so claude --dangerously-skip-permissions
//                                      doesn't gate the first launch on the
//                                      consent dialog
//
//   skills/agent-browser/SKILL.md      hidden auto-load skill teaching the
//                                      orch how to use its dedicated Chrome
//                                      via the security/agent-browser shims
func seedOrchClaudeDir(orchDir string) error {
	if err := seedClaudeJSON(orchDir); err != nil {
		return err
	}
	if err := seedSettingsJSON(orchDir); err != nil {
		return err
	}
	if err := seedSkill(orchDir, "agent-browser", skillAgentBrowser); err != nil {
		return err
	}
	if err := seedSkill(orchDir, "artifact", skillArtifact); err != nil {
		return err
	}
	// director-core marketplace ships advanced-memory, advanced-knowledge,
	// and director-agents. Idempotent: claude CLI no-ops on duplicates.
	// Runs synchronously so it completes before the spawn returns —
	// running in a goroutine gets killed when this CLI process exits
	// (typically before the second `claude plugin install` completes).
	// Adds ~5-10s to the first spawn of an orch; subsequent spawns are
	// instant since every step short-circuits on "already".
	seedDirectorCore(orchDir)
	return nil
}

// seedDispatcherClaudeDir is a stripped-down version of
// seedOrchClaudeDir for dispatchers: onboarding bypass yes, plugins
// no, agent-browser/artifact skills no. The dispatcher is a router,
// not a worker — it should have no skills/plugins of its own so the
// global ~/.claude inventory doesn't bleed into its prompt.
func seedDispatcherClaudeDir(dir string) error {
	if err := seedClaudeJSON(dir); err != nil {
		return err
	}
	return seedSettingsJSON(dir)
}

// directorCoreMarketplaceURL is the public marketplace.json that ships
// with Director. Hosted on superchargeclaudecode.com; the source
// plugins live in gkkirsch/gkkirsch-claude-plugins.
const directorCoreMarketplaceURL = "https://superchargeclaudecode.com/api/marketplaces/director-core/marketplace.json"

// directorCorePlugins is the auto-install list. Keep this in sync with
// the marketplace contents — adding a plugin to director-core means
// adding it here.
var directorCorePlugins = []string{
	"advanced-memory@director-core",
	"advanced-knowledge@director-core",
	"director-agents@director-core",
	"tasks@director-core",
}

// seedDirectorCore registers the director-core marketplace and installs
// its plugins into orchDir. Runs synchronously so the orch's first
// spawn doesn't return before plugins are in place. Errors are logged,
// never returned — users can recover by adding the marketplace manually.
func seedDirectorCore(orchDir string) {
	env := append(os.Environ(), "CLAUDE_CONFIG_DIR="+orchDir)

	add := exec.Command("claude", "plugin", "marketplace", "add", directorCoreMarketplaceURL)
	add.Env = env
	if out, err := add.CombinedOutput(); err != nil {
		// "already exists" is the expected path on every spawn after
		// the first — don't spam stderr with it.
		if !bytes.Contains(out, []byte("already")) {
			fmt.Fprintf(os.Stderr, "roster: director-core add: %v — %s\n", err, strings.TrimSpace(string(out)))
		}
	}
	for i, spec := range directorCorePlugins {
		// Brief gap between installs — claude plugin install creates
		// a temp_git_<ts> dir under plugins/cache; back-to-back calls
		// against the same dir occasionally race and one of them
		// silently fails to persist installed_plugins.json. The 250ms
		// breather is cheap and reliable.
		if i > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		install := exec.Command("claude", "plugin", "install", spec)
		install.Env = env
		out, err := install.CombinedOutput()
		if err != nil && !bytes.Contains(out, []byte("already")) {
			fmt.Fprintf(os.Stderr, "roster: director-core install %s: %v — %s\n", spec, err, strings.TrimSpace(string(out)))
		}
	}
}

// seedSkill writes a roster-bundled skill into <orch_dir>/skills/<name>/
// SKILL.md. Idempotent: skips writing when on-disk content matches the
// embedded copy. Skills with `hidden: true` auto-load on intent match.
func seedSkill(orchDir, name string, content []byte) error {
	dir := filepath.Join(orchDir, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dir, "SKILL.md")
	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, content) {
		return nil
	}
	return os.WriteFile(target, content, 0o644)
}

func seedClaudeJSON(orchDir string) error {
	target := filepath.Join(orchDir, ".claude.json")
	existing := map[string]any{}
	if b, err := os.ReadFile(target); err == nil {
		_ = json.Unmarshal(b, &existing)
	}

	home, _ := os.UserHomeDir()
	homeTrusted := isHomeTrusted(existing, home)

	if existing["theme"] != nil &&
		existing["hasCompletedOnboarding"] == true &&
		existing["oauthAccount"] != nil &&
		homeTrusted {
		return nil
	}

	user := readUserClaudeJSON()
	if existing["theme"] == nil {
		if t, ok := user["theme"]; ok {
			existing["theme"] = t
		} else {
			existing["theme"] = "dark"
		}
	}
	existing["hasCompletedOnboarding"] = true
	if v, ok := user["lastOnboardingVersion"]; ok && existing["lastOnboardingVersion"] == nil {
		existing["lastOnboardingVersion"] = v
	}
	// Profile metadata travels with the auth so the orch's main loop
	// can render "logged in as <user>" without a profile fetch.
	if existing["oauthAccount"] == nil {
		if v, ok := user["oauthAccount"]; ok {
			existing["oauthAccount"] = v
		}
	}

	// Pre-accept the workspace-trust dialog for the user's home dir.
	// claude-code's trust check walks UP from cwd through every parent
	// looking for a trusted ancestor, so a single HOME entry covers the
	// dispatcher's cwd + every orch space + every worker space, present
	// and future. Without this, every spawned claude stalls on the
	// "Do you trust this folder?" prompt forever (— the
	// --dangerously-skip-permissions flag only relaxes tool-execution
	// permissions, not workspace trust). When that happens, roster's
	// "wait for ready" times out and the caller retries the spawn,
	// leaving an orphan tmux session per attempt — that's the
	// "100s of tmux panes on first launch" report.
	if home != "" {
		projects, _ := existing["projects"].(map[string]any)
		if projects == nil {
			projects = map[string]any{}
			existing["projects"] = projects
		}
		homeProj, _ := projects[home].(map[string]any)
		if homeProj == nil {
			homeProj = map[string]any{}
			projects[home] = homeProj
		}
		homeProj["hasTrustDialogAccepted"] = true
	}

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(target, out, 0o600)
}

// isHomeTrusted reports whether the existing .claude.json already
// has hasTrustDialogAccepted=true for the user's home directory.
// Used as part of the seed idempotency check so we don't rewrite
// .claude.json on every spawn after the first.
func isHomeTrusted(existing map[string]any, home string) bool {
	if home == "" {
		return false
	}
	projects, ok := existing["projects"].(map[string]any)
	if !ok {
		return false
	}
	proj, ok := projects[home].(map[string]any)
	if !ok {
		return false
	}
	v, _ := proj["hasTrustDialogAccepted"].(bool)
	return v
}

func seedSettingsJSON(orchDir string) error {
	target := filepath.Join(orchDir, "settings.json")
	existing := map[string]any{}
	if b, err := os.ReadFile(target); err == nil {
		_ = json.Unmarshal(b, &existing)
	}
	if existing["skipDangerousModePermissionPrompt"] == true {
		return nil
	}
	existing["skipDangerousModePermissionPrompt"] = true
	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(target, out, 0o644)
}

// readUserClaudeJSON loads ~/.claude/.claude.json best-effort.
// Returns an empty map on any error.
func readUserClaudeJSON() map[string]any {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", ".claude.json"))
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// cmdAuth manages the Director-managed long-lived OAuth token used
// by every spawned orch. JSON-only output for the UI to consume.
//
//   roster auth status                 → {"configured": bool}
//   roster auth set <token>            → store
//   roster auth clear                  → remove
//
// Subcommand "set" reads the token from stdin if no positional arg is
// given, so callers don't have to put it on the command line where
// shell history can capture it.
func cmdAuth(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: roster auth status|set|clear")
	}
	switch args[0] {
	case "status":
		tok, err := directorOAuthToken()
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"configured": tok != "",
		})
	case "set":
		var token string
		if len(args) >= 2 {
			token = strings.TrimSpace(args[1])
		} else {
			b, err := os.ReadFile("/dev/stdin")
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			token = strings.TrimSpace(string(b))
		}
		if !strings.HasPrefix(token, "sk-ant-oat") {
			return fmt.Errorf("token doesn't look like a setup-token (expected to start with 'sk-ant-oat')")
		}
		// add-generic-password -U updates in place if present.
		cmd := exec.Command("/usr/bin/security", "add-generic-password",
			"-s", directorOAuthTokenService,
			"-a", os.Getenv("USER"),
			"-w", token,
			"-U")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("write keychain: %v — %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintln(os.Stdout, `{"configured": true}`)
		return nil
	case "clear":
		// security exits 44 if not present; treat as success.
		_ = exec.Command("/usr/bin/security", "delete-generic-password",
			"-s", directorOAuthTokenService,
			"-a", os.Getenv("USER")).Run()
		fmt.Fprintln(os.Stdout, `{"configured": false}`)
		return nil
	}
	return fmt.Errorf("unknown auth subcommand %q (want status|set|clear)", args[0])
}

