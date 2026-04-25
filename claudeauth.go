package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Per-orch CLAUDE_CONFIG_DIR isolation breaks first-launch in two
// places: the theme picker and the OAuth login picker. We solve both
// at provision time:
//
//  1. Seed .claude.json with theme + hasCompletedOnboarding so
//     interactiveHelpers.tsx:111 short-circuits the Onboarding
//     component.
//
//  2. Install a `security` shim that strips the per-orch hash from
//     `Claude Code-credentials-<hex>` so every orch reads/writes the
//     same canonical keychain entry as the user's ~/.claude. This
//     keeps refresh-token rotation coordinated across orchs and the
//     user's main session.
//
// See:
//   - claude-code-internals: utils/secureStorage/macOsKeychainHelpers.ts
//     (service-name hash derivation)
//   - claude-code-internals: utils/secureStorage/fallbackStorage.ts
//     (keychain → plaintext fallback chain — we don't depend on
//     plaintext, but the shim's correctness rides on the keychain
//     path being the only one ever used)

//go:embed assets/security
var securityShim []byte

//go:embed assets/skill-agent-browser.md
var skillAgentBrowser []byte

// installSecurityShim writes the shim into <bin>/security and ALSO
// symlinks it from ~/.local/bin/security so it lands on the user's
// regular PATH ahead of /usr/bin/security. tmux set-environment
// doesn't reliably propagate PATH to new windows (default-shell zsh
// rebuilds PATH on startup), so the canonical install must already be
// somewhere zsh's profile leaves on PATH.
//
// The shim is selective — it only rewrites service names matching
// Claude Code's hashed-credentials pattern. Everything else passes
// through unchanged, so installing globally is safe.
func installSecurityShim() (string, error) {
	canonical, err := installSecurityShimCanonical()
	if err != nil {
		return "", err
	}
	if err := ensureLocalBinSymlink("security", canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func installSecurityShimCanonical() (string, error) {
	dir, err := browserBinDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "security")
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, securityShim) {
		_ = os.Chmod(path, 0o755)
		return path, nil
	}
	if err := os.WriteFile(path, securityShim, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

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
	return seedAgentBrowserSkill(orchDir)
}

// seedAgentBrowserSkill writes the agent-browser skill into the per-orch
// skills dir if missing or out of date. The skill auto-loads on
// browser-related tasks (`hidden: true`) and overrides the upstream
// vercel-labs/agent-browser instructions with roster-specific rules
// (must use $AGENT_BROWSER_CDP, never 9222, never run `install`).
func seedAgentBrowserSkill(orchDir string) error {
	dir := filepath.Join(orchDir, "skills", "agent-browser")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dir, "SKILL.md")
	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, skillAgentBrowser) {
		return nil
	}
	return os.WriteFile(target, skillAgentBrowser, 0o644)
}

func seedClaudeJSON(orchDir string) error {
	target := filepath.Join(orchDir, ".claude.json")
	existing := map[string]any{}
	if b, err := os.ReadFile(target); err == nil {
		_ = json.Unmarshal(b, &existing)
	}
	if existing["theme"] != nil && existing["hasCompletedOnboarding"] == true && existing["oauthAccount"] != nil {
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

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(target, out, 0o600)
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

// userKeychainHasClaudeCreds is a fail-loud preflight: refuse to spawn
// an orch if the user hasn't logged in to Claude on this machine, so
// the failure mode is "log in first" instead of "orch boots and dies
// silently the first time it tries to call the API."
func userKeychainHasClaudeCreds() bool {
	out, err := runRealSecurity("find-generic-password", "-s", "Claude Code-credentials", "-w")
	if err != nil {
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
}

// runRealSecurity invokes /usr/bin/security directly so we never
// recurse through the shim regardless of PATH ordering.
func runRealSecurity(args ...string) ([]byte, error) {
	cmd := exec.Command("/usr/bin/security", args...)
	var out bytes.Buffer
	var errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("security %v: %w (stderr: %s)", args, err, errb.String())
	}
	return out.Bytes(), nil
}
