package main

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed prompts/dispatcher.md prompts/orchestrator.md prompts/worker.md
var embeddedPrompts embed.FS

// promptData is the template context passed to all prompt templates.
type promptData struct {
	ID          string
	Parent      string
	Description string
}

// promptsDir returns the per-user writable prompts directory under XDG_CONFIG_HOME
// (or $HOME/.config). Edits here override the embedded defaults.
func promptsDir() (string, error) {
	if d := os.Getenv("ROSTER_PROMPTS_DIR"); d != "" {
		return d, nil
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "roster", "prompts"), nil
}

// loadPromptTemplate reads the raw template text for a kind, preferring
// the user-editable file on disk, falling back to the embedded default.
func loadPromptTemplate(kind string) (string, error) {
	dir, err := promptsDir()
	if err != nil {
		return "", err
	}
	diskPath := filepath.Join(dir, kind+".md")
	if b, err := os.ReadFile(diskPath); err == nil {
		return string(b), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// Fall back to embedded default.
	b, err := embeddedPrompts.ReadFile("prompts/" + kind + ".md")
	if err != nil {
		return "", fmt.Errorf("no built-in prompt for kind %q", kind)
	}
	return string(b), nil
}

// renderPrompt returns the final system prompt text for the given agent.
// Templates use Go text/template syntax; available fields are promptData.
func renderPrompt(kind string, data promptData) (string, error) {
	raw, err := loadPromptTemplate(kind)
	if err != nil {
		return "", err
	}
	// `missingkey=zero` so an orchestrator template with {{.Parent}} renders
	// as empty when Parent is "" rather than failing.
	tmpl, err := template.New(kind).Option("missingkey=zero").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s template: %w", kind, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render %s template: %w", kind, err)
	}
	return buf.String(), nil
}

// materializeDefaultPrompts copies the embedded defaults into the user's
// prompts directory. Used by `roster init`. --force overwrites existing files.
func materializeDefaultPrompts(force bool) ([]string, error) {
	dir, err := promptsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for _, kind := range KnownKinds {
		dst := filepath.Join(dir, kind+".md")
		if _, err := os.Stat(dst); err == nil && !force {
			continue // keep user's edits
		}
		src, err := embeddedPrompts.ReadFile("prompts/" + kind + ".md")
		if err != nil {
			return written, err
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			return written, err
		}
		written = append(written, dst)
	}
	return written, nil
}
