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

func TestBuildSystemAppendix(t *testing.T) {
	// With parent
	app := buildSystemAppendix("worker-1", "orch-a", "worker")
	if !strings.Contains(app, "worker-1") || !strings.Contains(app, "orch-a") {
		t.Errorf("appendix should mention id and parent: %s", app)
	}
	if !strings.Contains(app, "roster notify orch-a") {
		t.Errorf("appendix should instruct on notify syntax: %s", app)
	}
	// Without parent (top of tree)
	app2 := buildSystemAppendix("dispatch", "", "dispatcher")
	if !strings.Contains(app2, "no parent") {
		t.Errorf("top-level appendix should say no parent: %s", app2)
	}
}

func TestMergeAppendSystem(t *testing.T) {
	// No existing --append-system: should add one.
	in := []string{"--model", "sonnet"}
	out := mergeAppendSystem(in, "EXTRA")
	if len(out) != 4 || out[2] != "--append-system" || out[3] != "EXTRA" {
		t.Errorf("add: got %v", out)
	}
	// Existing --append-system: should concatenate.
	in = []string{"--model", "sonnet", "--append-system", "USER"}
	out = mergeAppendSystem(in, "EXTRA")
	if len(out) != 4 || out[3] != "USER\n\nEXTRA" {
		t.Errorf("merge: got %v", out)
	}
	// Existing --append-system= (= form).
	in = []string{"--model", "sonnet", "--append-system=USER"}
	out = mergeAppendSystem(in, "EXTRA")
	if len(out) != 3 || out[2] != "--append-system=USER\n\nEXTRA" {
		t.Errorf("merge =form: got %v", out)
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
