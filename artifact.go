package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Per-orch artifacts: little Vite + React + Tailwind apps each orch can
// scaffold and edit. fleetview spawns the dev server lazily, points an
// iframe at it, and the orch sees its work render live via Vite HMR.
//
// On disk:
//
//   <orch_claude_dir>/artifacts/<aid>/
//     ├── package.json   (vite + react + tailwind)
//     ├── vite.config.ts
//     ├── tsconfig.json
//     ├── index.html
//     ├── src/
//     │   ├── main.tsx
//     │   ├── App.tsx
//     │   └── styles.css
//     └── .roster-artifact   (sidecar: type, title, port, created)

//go:embed all:assets/artifact-template
var artifactTemplate embed.FS

// ArtifactSidecar is the metadata file fleetview reads to learn about
// each artifact. Kept tiny — port is derived deterministically, the
// dev server's PID is a runtime concern that lives in fleetview.
type ArtifactSidecar struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	Title   string    `json:"title,omitempty"`
	Port    int       `json:"port"`
	Created time.Time `json:"created"`
}

const (
	artifactPortBase  = 5170
	artifactPortRange = 100
)

// artifactPortFor returns the deterministic dev-server port for one
// orch:aid pair. Range chosen to sit past Vite's 5173 default and
// below most app dev ports.
func artifactPortFor(orchID, aid string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(orchID + ":" + aid))
	return artifactPortBase + int(h.Sum32()%uint32(artifactPortRange))
}

// artifactsRootForOrch returns <orch_claude_dir>/artifacts (created).
func artifactsRootForOrch(orchID string) (string, error) {
	dir, err := orchestratorClaudeDir(orchID)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

// artifactDirFor returns <orch_claude_dir>/artifacts/<aid>.
func artifactDirFor(orchID, aid string) (string, error) {
	root, err := artifactsRootForOrch(orchID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, aid), nil
}

// createArtifact scaffolds a fresh artifact dir from the embedded
// template and writes the sidecar. Refuses if the dir already exists
// — replacement is the operator's call (delete then create).
func createArtifact(orchID, aid, kind, title string) (*ArtifactSidecar, string, error) {
	if !validArtifactID(aid) {
		return nil, "", fmt.Errorf("invalid artifact id %q (use letters, digits, dash, underscore)", aid)
	}
	if kind == "" {
		kind = "react-tailwind"
	}
	dir, err := artifactDirFor(orchID, aid)
	if err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(dir); err == nil {
		return nil, dir, fmt.Errorf("artifact %s already exists at %s", aid, dir)
	}
	if err := copyEmbeddedTree(artifactTemplate, "assets/artifact-template", dir); err != nil {
		return nil, dir, fmt.Errorf("scaffold: %w", err)
	}
	sidecar := &ArtifactSidecar{
		ID:      aid,
		Type:    kind,
		Title:   title,
		Port:    artifactPortFor(orchID, aid),
		Created: time.Now().UTC(),
	}
	if err := writeArtifactSidecar(dir, sidecar); err != nil {
		return nil, dir, err
	}
	return sidecar, dir, nil
}

// listArtifacts enumerates an orch's artifacts by reading sidecars.
func listArtifacts(orchID string) ([]ArtifactSidecar, error) {
	root, err := artifactsRootForOrch(orchID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []ArtifactSidecar
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := readArtifactSidecar(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

func readArtifactSidecar(dir string) (*ArtifactSidecar, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".roster-artifact"))
	if err != nil {
		return nil, err
	}
	var s ArtifactSidecar
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeArtifactSidecar(dir string, s *ArtifactSidecar) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ".roster-artifact"), b, 0o644)
}

// copyEmbeddedTree walks an embed.FS subtree and writes every file out
// to dst. Preserves dir structure; skips the embed.FS root itself.
func copyEmbeddedTree(src embed.FS, root, dst string) error {
	return fs.WalkDir(src, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := src.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// validArtifactID — lowercase letters, digits, dash, underscore.
// 1-64 chars. Keeps URL paths clean and avoids surprise on disk.
func validArtifactID(s string) bool {
	if l := len(s); l == 0 || l > 64 {
		return false
	}
	for _, r := range s {
		ok := r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// --- CLI dispatch ----------------------------------------------------------

func cmdArtifact(args []string) error {
	if len(args) < 1 {
		return artifactUsage()
	}
	switch args[0] {
	case "create":
		return cmdArtifactCreate(args[1:])
	case "list":
		return cmdArtifactList(args[1:])
	case "path":
		return cmdArtifactPath(args[1:])
	case "-h", "--help", "help":
		return artifactUsage()
	default:
		return fmt.Errorf("unknown artifact subcommand %q\n%s", args[0], artifactUsageText)
	}
}

const artifactUsageText = `usage:
  roster artifact create <orch> <aid> [--type react-tailwind] [--title "..."]
         Scaffold a new artifact (Vite + React + Tailwind) inside the orch's
         CLAUDE_CONFIG_DIR/artifacts/<aid>/. Prints the dir path on success.

  roster artifact list <orch>
         List artifacts for an orch (id, type, port, created).

  roster artifact path <orch> <aid>
         Print the on-disk path of an artifact.
`

func artifactUsage() error {
	fmt.Print(artifactUsageText)
	return nil
}

func cmdArtifactCreate(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: roster artifact create <orch> <aid> [--type T] [--title \"...\"]")
	}
	orch := args[0]
	aid := args[1]
	fs := flag.NewFlagSet("artifact create", flag.ContinueOnError)
	kind := fs.String("type", "react-tailwind", "artifact type (currently only react-tailwind)")
	title := fs.String("title", "", "human-readable title for the artifact")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if _, err := loadAgent(orch); err != nil {
		return fmt.Errorf("artifact create: %w", err)
	}
	side, dir, err := createArtifact(orch, aid, *kind, *title)
	if err != nil {
		return fmt.Errorf("artifact create: %w", err)
	}
	fmt.Println(dir)
	fmt.Fprintf(os.Stderr, "scaffolded %s (type=%s, port=%d) in %s\n", side.ID, side.Type, side.Port, dir)
	return nil
}

func cmdArtifactList(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roster artifact list <orch>")
	}
	orch := args[0]
	if _, err := loadAgent(orch); err != nil {
		return fmt.Errorf("artifact list: %w", err)
	}
	items, err := listArtifacts(orch)
	if err != nil {
		return fmt.Errorf("artifact list: %w", err)
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "no artifacts")
		return nil
	}
	for _, a := range items {
		title := a.Title
		if title == "" {
			title = "-"
		}
		fmt.Printf("%-20s %-14s :%d  %s\n", a.ID, a.Type, a.Port, title)
	}
	return nil
}

func cmdArtifactPath(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: roster artifact path <orch> <aid>")
	}
	orch, aid := args[0], args[1]
	dir, err := artifactDirFor(orch, aid)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("artifact path: %w", err)
	}
	fmt.Println(dir)
	return nil
}

