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
//
// Space + ClaudeDir are resolved to absolute paths at render time so
// templates can reference the orch's two directories literally,
// without depending on env-var expansion at runtime. This matters for
// "install a skill into your claude dir"-style instructions: when the
// prompt says `$CLAUDE_CONFIG_DIR/skills/...`, the orch sometimes
// invents a project-local `.claude/skills/` instead. Inlining the
// resolved path removes that ambiguity.
type promptData struct {
	ID          string
	Parent      string
	Description string
	Space       string // absolute path to <data>/<orch-id>/  ($DIRECTOR_SPACE)
	ClaudeDir   string // absolute path to per-orch CLAUDE_CONFIG_DIR
}

// Edits in promptsDir() override the embedded defaults.
func promptsDir() (string, error) {
	return rosterPath("ROSTER_PROMPTS_DIR", "XDG_CONFIG_HOME", ".config", "prompts")
}

// readEmbeddedPrompt returns the built-in template for a kind or a
// friendly error if none is bundled.
func readEmbeddedPrompt(kind string) ([]byte, error) {
	b, err := embeddedPrompts.ReadFile("prompts/" + kind + ".md")
	if err != nil {
		return nil, fmt.Errorf("no built-in prompt for kind %q", kind)
	}
	return b, nil
}

// loadPromptTemplate prefers the user-editable file on disk, falling back
// to the embedded default.
func loadPromptTemplate(kind string) (string, error) {
	dir, err := promptsDir()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, kind+".md"))
	if err == nil {
		return string(b), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	b, err = readEmbeddedPrompt(kind)
	if err != nil {
		return "", err
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

// ensurePromptOnDisk writes the embedded default for kind to disk if it's
// missing (or if force is set). Returns the destination path and whether
// a write happened.
func ensurePromptOnDisk(kind string, force bool) (path string, wrote bool, err error) {
	dir, err := promptsDir()
	if err != nil {
		return "", false, err
	}
	dst := filepath.Join(dir, kind+".md")
	if _, err := os.Stat(dst); err == nil && !force {
		return dst, false, nil
	}
	src, err := readEmbeddedPrompt(kind)
	if err != nil {
		return dst, false, err
	}
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		return dst, false, err
	}
	return dst, true, nil
}

// materializeDefaultPrompts writes each known kind via ensurePromptOnDisk.
func materializeDefaultPrompts(force bool) ([]string, error) {
	var written []string
	for _, kind := range KnownKinds {
		path, wrote, err := ensurePromptOnDisk(kind, force)
		if err != nil {
			return written, err
		}
		if wrote {
			written = append(written, path)
		}
	}
	return written, nil
}
