package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateID(t *testing.T) {
	good := []string{"plan-01", "orch_code", "A", "abc123", "x-y-z"}
	bad := []string{"", "-foo", "foo bar", "foo/bar", "foo.bar", "-"}
	for _, id := range good {
		if err := validateID(id); err != nil {
			t.Errorf("validateID(%q): unexpected error %v", id, err)
		}
	}
	for _, id := range bad {
		if err := validateID(id); err == nil {
			t.Errorf("validateID(%q): expected error, got nil", id)
		}
	}
}

func TestValidateKind(t *testing.T) {
	for _, k := range []string{"dispatcher", "orchestrator", "worker"} {
		if err := validateKind(k); err != nil {
			t.Errorf("validateKind(%q): %v", k, err)
		}
	}
	if err := validateKind("mystery"); err == nil {
		t.Error("validateKind(mystery): expected error")
	}
}

// TestStoreRoundtrip uses ROSTER_DIR to isolate.
func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROSTER_DIR", filepath.Join(dir, "agents"))

	now := time.Now().UTC()
	a := &Agent{
		ID:          "test-agent-1",
		Kind:        "worker",
		Parent:      "orch",
		Description: "does things",
		SessionUUID: "abc-def",
		SpawnArgs:   []string{"--model", "sonnet"},
		Cwd:         "/tmp",
		Target:      "test-agent-1:cc",
		Created:     now,
		LastSeen:    now,
	}
	if err := saveAgent(a); err != nil {
		t.Fatal(err)
	}
	if !agentExists("test-agent-1") {
		t.Fatal("agentExists should return true after save")
	}
	loaded, err := loadAgent("test-agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != a.ID || loaded.Kind != a.Kind || loaded.Description != a.Description {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", loaded, a)
	}
	if len(loaded.SpawnArgs) != 2 || loaded.SpawnArgs[0] != "--model" {
		t.Fatalf("SpawnArgs roundtrip: got %v", loaded.SpawnArgs)
	}
	all, err := listAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "test-agent-1" {
		t.Fatalf("listAgents: got %+v", all)
	}
	if err := deleteAgent("test-agent-1"); err != nil {
		t.Fatal(err)
	}
	if agentExists("test-agent-1") {
		t.Fatal("agentExists should be false after delete")
	}
}

func TestChildrenOf(t *testing.T) {
	all := []*Agent{
		{ID: "root"},
		{ID: "mid-a", Parent: "root"},
		{ID: "mid-b", Parent: "root"},
		{ID: "leaf", Parent: "mid-a"},
	}
	if kids := childrenOf("root", all); len(kids) != 2 {
		t.Errorf("root children: got %d, want 2", len(kids))
	}
	if kids := childrenOf("mid-a", all); len(kids) != 1 || kids[0].ID != "leaf" {
		t.Errorf("mid-a children: got %+v", kids)
	}
	if kids := childrenOf("leaf", all); len(kids) != 0 {
		t.Errorf("leaf children: got %+v", kids)
	}
}

func TestRenderPrompt(t *testing.T) {
	// Embedded templates should render for each known kind without error.
	for _, kind := range KnownKinds {
		out, err := renderPrompt(kind, promptData{
			ID:          "test-" + kind,
			Parent:      "some-parent",
			Description: "the test agent",
		})
		if err != nil {
			t.Fatalf("renderPrompt(%s): %v", kind, err)
		}
		if !strings.Contains(out, "test-"+kind) {
			t.Errorf("rendered %s prompt missing ID placeholder", kind)
		}
		if kind != "dispatcher" && !strings.Contains(out, "some-parent") {
			t.Errorf("rendered %s prompt missing Parent placeholder", kind)
		}
	}
}

func TestMaterializeDefaultPrompts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROSTER_PROMPTS_DIR", dir)
	written, err := materializeDefaultPrompts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != len(KnownKinds) {
		t.Fatalf("expected %d written, got %d", len(KnownKinds), len(written))
	}
	// Re-run without --force should skip.
	written2, err := materializeDefaultPrompts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written2) != 0 {
		t.Errorf("second run without --force should write nothing, got %d", len(written2))
	}
	// Force overwrites.
	written3, err := materializeDefaultPrompts(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(written3) != len(KnownKinds) {
		t.Errorf("force should rewrite all, got %d", len(written3))
	}
}

func TestPromptOverrideFromDisk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROSTER_PROMPTS_DIR", dir)
	// Put a custom template on disk.
	custom := "OVERRIDE for {{.ID}} parent={{.Parent}}"
	if err := os.WriteFile(dir+"/worker.md", []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := renderPrompt("worker", promptData{ID: "x", Parent: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "OVERRIDE for x parent=y" {
		t.Fatalf("disk override not honored, got: %s", out)
	}
}

func TestShortDesc(t *testing.T) {
	if got := shortDesc("hello", 20); got != "hello" {
		t.Errorf("got %q", got)
	}
	if got := shortDesc("this is a very long description", 10); got != "this is a…" {
		t.Errorf("got %q", got)
	}
	if got := shortDesc("line one\nline two", 20); !strings.Contains(got, "line one line two") {
		t.Errorf("newlines should collapse; got %q", got)
	}
}

// Smoke: binary builds and prints help.
func TestHelp(t *testing.T) {
	_ = os.Args
	// Nothing to exercise here at the test level; the Makefile `test`
	// target plus `go vet` cover it. This test just verifies the package
	// compiles cleanly when run under `go test`.
}
