package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// --- spawn ------------------------------------------------------------------

func cmdSpawn(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roster spawn <id> [--kind K] [--parent P] [--description \"...\"] -- <camux-spawn-flags>")
	}
	id := args[0]
	if err := validateID(id); err != nil {
		return err
	}

	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	kind := fs.String("kind", "worker", strings.Join(KnownKinds, " | "))
	parent := fs.String("parent", "", "id of the agent that spawned this one")
	description := fs.String("description", "", "rolling summary of what this agent is for / has done")
	displayName := fs.String("display-name", "", "human-friendly UI label (e.g. \"Host Reply\"); falls back to a humanized id when empty")
	rawTimeout := fs.Duration("timeout", 90*time.Second, "how long to wait for the Claude TUI to become ready")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	camuxFlags := fs.Args() // everything after --

	if err := validateKind(*kind); err != nil {
		return err
	}
	if agentExists(id) {
		return fmt.Errorf("spawn: agent %q already exists (use `roster resume %s` or `roster forget %s` first)", id, id, id)
	}
	if *parent != "" && !agentExists(*parent) {
		return fmt.Errorf("spawn: --parent %q not in roster", *parent)
	}

	systemPrompt, err := renderPrompt(*kind, promptData{
		ID:          id,
		Parent:      *parent,
		Description: *description,
	})
	if err != nil {
		return fmt.Errorf("spawn: render system prompt: %w", err)
	}

	// Prepend --system-prompt so any user-provided one later in camuxFlags
	// wins (claude's CLI: later flag overrides earlier).
	baseFlags := []string{"--system-prompt", systemPrompt}
	// Dispatchers run on the latest Sonnet — fast routing, the
	// orchestrators it spawns can use whatever model they need.
	if *kind == "dispatcher" {
		baseFlags = append(baseFlags, "--model", "claude-sonnet-4-6")
	}
	camuxFlags = append(baseFlags, camuxFlags...)

	session := id

	// Isolate this agent's ~/.claude. Each agent kind gets its own dir:
	//   orchestrator → own dir
	//   worker       → inherits its orchestrator's dir
	//   dispatcher   → own dir (no plugins/skills — pure router)
	// Pre-creates the tmux session and sets CLAUDE_CONFIG_DIR on it so
	// the upcoming new-window picks it up.
	if _, err := prepareClaudeIsolation(*kind, id, *parent, session); err != nil {
		return fmt.Errorf("spawn: claude dir isolation: %w", err)
	}
	if _, err := prepareBrowserIsolation(*kind, id, *parent, session); err != nil {
		return fmt.Errorf("spawn: browser env: %w", err)
	}

	spawnArgs := append([]string{"spawn", session}, camuxFlags...)
	spawnArgs = append(spawnArgs, "--timeout", rawTimeout.String())

	out, err := runCamux(spawnArgs...)
	if err != nil {
		return fmt.Errorf("spawn: camux spawn failed: %w", err)
	}
	target := strings.TrimSpace(strings.Split(out, "\n")[0])
	if target == "" {
		return fmt.Errorf("spawn: camux spawn returned empty target, stdout=%q", out)
	}

	// Pull the Claude session UUID via camux info (best-effort — if it
	// fails, we still save the record without it; resume won't work but
	// the record is still valid).
	var sessionUUID, cwd string
	if info, err := camuxInfo(target); err == nil {
		sessionUUID = info.SessionID
		cwd = info.Cwd
	}

	now := time.Now().UTC()
	a := &Agent{
		ID:          id,
		Kind:        *kind,
		Parent:      *parent,
		DisplayName: strings.TrimSpace(*displayName),
		Description: *description,
		SessionUUID: sessionUUID,
		SpawnArgs:   camuxFlags,
		Cwd:         cwd,
		Target:      target,
		Created:     now,
		LastSeen:    now,
	}
	if err := saveAgent(a); err != nil {
		return err
	}
	fmt.Println(target)
	return nil
}

// --- list -------------------------------------------------------------------

type listRow struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Parent      string `json:"parent,omitempty"`
	Target      string `json:"target,omitempty"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	Created     string `json:"created"`
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	kind := fs.String("kind", "", "filter by kind")
	parent := fs.String("parent", "", "filter by parent id")
	status := fs.String("status", "", "filter by live status (ready, streaming, stopped, ...)")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	all, err := listAgents()
	if err != nil {
		return err
	}
	var rows []listRow
	for _, a := range all {
		if *kind != "" && a.Kind != *kind {
			continue
		}
		if *parent != "" && a.Parent != *parent {
			continue
		}
		st := camuxStatus(a.Target)
		if *status != "" && st != *status {
			continue
		}
		rows = append(rows, listRow{
			ID:          a.ID,
			Kind:        a.Kind,
			Parent:      a.Parent,
			Target:      a.Target,
			Status:      st,
			Description: shortDesc(a.Description, 60),
			Created:     a.Created.Format(time.RFC3339),
		})
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("(no agents)")
		return nil
	}
	for _, r := range rows {
		parent := "-"
		if r.Parent != "" {
			parent = r.Parent
		}
		fmt.Printf("%-20s  %-13s  parent=%-15s  status=%-10s  %s\n",
			r.ID, r.Kind, parent, r.Status, r.Description)
	}
	return nil
}

func shortDesc(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// --- describe ---------------------------------------------------------------

func cmdDescribe(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roster describe <id> [--json] [--tail N]")
	}
	id := args[0]
	fs := flag.NewFlagSet("describe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit JSON")
	tail := fs.Int("tail", 0, "if live, also include last N lines of the pane")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	a, err := loadAgent(id)
	if err != nil {
		return err
	}
	type out struct {
		*Agent
		Status string `json:"status"`
		Tail   string `json:"tail,omitempty"`
	}
	o := out{Agent: a, Status: camuxStatus(a.Target)}
	if *tail > 0 && (o.Status == "ready" || o.Status == "streaming" || o.Status == "starting") {
		if tOut, err := runAmux("capture", a.Target, "--lines", fmt.Sprint(*tail)); err == nil {
			o.Tail = tOut
		}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(o)
	}
	fmt.Printf("id:          %s\n", a.ID)
	fmt.Printf("kind:        %s\n", a.Kind)
	if a.Parent != "" {
		fmt.Printf("parent:      %s\n", a.Parent)
	}
	fmt.Printf("status:      %s\n", o.Status)
	if a.Target != "" {
		fmt.Printf("target:      %s\n", a.Target)
	}
	if a.SessionUUID != "" {
		fmt.Printf("session_id:  %s\n", a.SessionUUID)
	}
	if a.Cwd != "" {
		fmt.Printf("cwd:         %s\n", a.Cwd)
	}
	fmt.Printf("created:     %s\n", a.Created.Format(time.RFC3339))
	if !a.LastSeen.IsZero() {
		fmt.Printf("last_seen:   %s\n", a.LastSeen.Format(time.RFC3339))
	}
	if a.Description != "" {
		fmt.Printf("description:\n  %s\n", strings.ReplaceAll(a.Description, "\n", "\n  "))
	}
	if o.Tail != "" {
		fmt.Printf("---- tail (%d) ----\n%s\n", *tail, o.Tail)
	}
	return nil
}

// --- target -----------------------------------------------------------------

func cmdTarget(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: roster target <id>")
	}
	a, err := loadAgent(args[0])
	if err != nil {
		return err
	}
	if a.Target == "" {
		return fmt.Errorf("target: %s has no known target (stopped)", a.ID)
	}
	fmt.Println(a.Target)
	return nil
}

// --- tree -------------------------------------------------------------------

func cmdTree(args []string) error {
	all, err := listAgents()
	if err != nil {
		return err
	}
	// Find roots (parent == "" or parent not in the set).
	ids := make(map[string]bool, len(all))
	for _, a := range all {
		ids[a.ID] = true
	}
	var roots []*Agent
	for _, a := range all {
		if a.Parent == "" || !ids[a.Parent] {
			roots = append(roots, a)
		}
	}
	if len(roots) == 0 {
		fmt.Println("(no agents)")
		return nil
	}
	for _, r := range roots {
		printTree(r, all, "", true)
	}
	return nil
}

func printTree(a *Agent, all []*Agent, prefix string, isRoot bool) {
	status := camuxStatus(a.Target)
	line := fmt.Sprintf("%s (%s) [%s]", a.ID, a.Kind, status)
	if a.Description != "" {
		line += "  — " + shortDesc(a.Description, 50)
	}
	if isRoot {
		fmt.Println(line)
	} else {
		fmt.Println(prefix + "└── " + line)
	}
	kids := childrenOf(a.ID, all)
	for i, k := range kids {
		newPrefix := prefix
		if isRoot {
			newPrefix = ""
		} else {
			newPrefix = prefix + "    "
		}
		_ = i
		printTree(k, all, newPrefix, false)
	}
}

// --- update -----------------------------------------------------------------

func cmdUpdate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roster update <id> [--description \"...\"] [--append \"...\"] [--display-name \"...\"]")
	}
	id := args[0]
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	description := fs.String("description", "", "replace description with this value")
	appendStr := fs.String("append", "", "append this text to description (with newline separator)")
	displayName := fs.String("display-name", "", "set the human-friendly UI label (pass empty to leave unchanged; use --display-name=\"\" to clear)")
	displayNameSet := false
	for _, a := range args[1:] {
		if a == "--display-name" || strings.HasPrefix(a, "--display-name=") {
			displayNameSet = true
			break
		}
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	a, err := loadAgent(id)
	if err != nil {
		return err
	}
	if *description != "" {
		a.Description = *description
	}
	if *appendStr != "" {
		if a.Description != "" {
			a.Description += "\n"
		}
		a.Description += *appendStr
	}
	if displayNameSet {
		a.DisplayName = strings.TrimSpace(*displayName)
	}
	if *description == "" && *appendStr == "" && !displayNameSet {
		return fmt.Errorf("update: need --description, --append, or --display-name")
	}
	a.LastSeen = time.Now().UTC()
	return saveAgent(a)
}

// --- search -----------------------------------------------------------------

func cmdSearch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roster search <query>")
	}
	query := strings.ToLower(strings.Join(args, " "))
	all, err := listAgents()
	if err != nil {
		return err
	}
	matches := 0
	for _, a := range all {
		if strings.Contains(strings.ToLower(a.Description), query) ||
			strings.Contains(strings.ToLower(a.ID), query) {
			fmt.Printf("%-20s  %-13s  %s\n", a.ID, a.Kind, shortDesc(a.Description, 80))
			matches++
		}
	}
	if matches == 0 {
		return fmt.Errorf("no matches for %q", query)
	}
	return nil
}

// --- resume -----------------------------------------------------------------

func cmdResume(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: roster resume <id>")
	}
	id := args[0]
	a, err := loadAgent(id)
	if err != nil {
		return err
	}
	// If already live, just print the target.
	if camuxStatus(a.Target) == "ready" {
		fmt.Println(a.Target)
		return nil
	}
	session := a.ID
	if _, err := prepareClaudeIsolation(a.Kind, a.ID, a.Parent, session); err != nil {
		return fmt.Errorf("resume: claude dir isolation: %w", err)
	}
	if _, err := prepareBrowserIsolation(a.Kind, a.ID, a.Parent, session); err != nil {
		return fmt.Errorf("resume: browser env: %w", err)
	}
	flags := append([]string{}, a.SpawnArgs...)
	if a.SessionUUID != "" {
		flags = append(flags, "--resume", a.SessionUUID)
	}
	spawnArgs := append([]string{"spawn", session}, flags...)
	out, err := runCamux(spawnArgs...)
	if err != nil && a.SessionUUID != "" {
		// `claude --resume <uuid>` fails when the JSONL was deleted /
		// rolled. Don't strand the orch — retry with a fresh session.
		// The new session_uuid gets persisted at the bottom of this
		// function via saveAgent so the next resume picks it up.
		fresh := append([]string{}, a.SpawnArgs...)
		spawnFresh := append([]string{"spawn", session}, fresh...)
		var freshErr error
		if out, freshErr = runCamux(spawnFresh...); freshErr == nil {
			a.SessionUUID = "" // forget the dead one
			err = nil
		} else {
			err = fmt.Errorf("%w; fresh-spawn fallback also failed: %v", err, freshErr)
		}
	}
	if err != nil {
		return fmt.Errorf("resume: camux spawn failed: %w", err)
	}
	target := strings.TrimSpace(strings.Split(out, "\n")[0])
	a.Target = target
	a.LastSeen = time.Now().UTC()
	if err := saveAgent(a); err != nil {
		return err
	}
	fmt.Println(target)
	return nil
}

// --- stop -------------------------------------------------------------------

func cmdStop(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: roster stop <id>")
	}
	id := args[0]
	a, err := loadAgent(id)
	if err != nil {
		return err
	}
	if a.Target != "" {
		// Extract session name from target (session:window) — amux kill
		// on the session kills the whole workspace.
		sess := strings.SplitN(a.Target, ":", 2)[0]
		_, _ = runAmux("kill", sess) // ignore error: might already be dead
	}
	a.Target = ""
	a.LastSeen = time.Now().UTC()
	return saveAgent(a)
}

// --- forget -----------------------------------------------------------------

func cmdForget(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: roster forget <id>")
	}
	id := args[0]
	a, err := loadAgent(id)
	if err != nil {
		return err
	}
	if a.Target != "" {
		sess := strings.SplitN(a.Target, ":", 2)[0]
		_, _ = runAmux("kill", sess)
	}
	return deleteAgent(id)
}

// --- notify -----------------------------------------------------------------

func cmdNotify(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: roster notify <to-id> \"<message>\" [--from <id>] [--wait-ready 30s]")
	}
	to := args[0]
	msg := args[1]
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "id of the sender (optional; prepended to the delivered message)")
	waitReady := fs.Duration("wait-ready", 30*time.Second, "how long to wait for recipient to be ready")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	a, err := loadAgent(to)
	if err != nil {
		return err
	}
	if a.Target == "" {
		return fmt.Errorf("notify: %s has no target (stopped). Run `roster resume %s` first.", to, to)
	}
	if err := waitForReady(a.Target, *waitReady); err != nil {
		return fmt.Errorf("notify: recipient %s not ready: %w", to, err)
	}
	// Format the delivered message. When the sender is a registered
	// roster agent, wrap it in a <from id="..."> envelope. Two reasons:
	//   1. Unambiguous parsing — the recipient (and the UI) can detect
	//      "this turn is a peer-relay, not raw user input" by tag, not
	//      by guessing where a "[from X]" prefix ends.
	//   2. The dashboard hides these envelopes from view; the user
	//      shouldn't see the cross-agent chatter, only their own
	//      messages and the dispatcher's replies.
	// Plain user input (no --from) passes through unchanged.
	delivered := msg
	if *from != "" {
		footer := ""
		if agentExists(*from) {
			footer = fmt.Sprintf(
				"\n\nTo respond, end your turn with: `roster notify %s \"<your reply>\" --from <your-agent-id>`. Plain text alone does NOT reach %s.",
				*from, *from,
			)
		}
		delivered = fmt.Sprintf("<from id=\"%s\">\n%s%s\n</from>", *from, msg, footer)
	}
	// Paste + submit directly via amux — camux `ask` would block waiting
	// for a reply, which isn't what notify semantics imply (fire-and-forget
	// arrival). We've already waited for ready via waitForReady above.
	cmd := exec.Command(amuxBin, "paste", a.Target, "--submit")
	cmd.Stdin = bytes.NewReader([]byte(delivered))
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify: paste failed: %s", strings.TrimSpace(errb.String()))
	}
	a.LastSeen = time.Now().UTC()
	return saveAgent(a)
}

// --- init / prompt ---------------------------------------------------------

// cmdInit materializes the embedded default prompt templates into the
// user's config dir. Idempotent; --force overwrites existing files.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "overwrite existing prompt files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	written, err := materializeDefaultPrompts(*force)
	if err != nil {
		return err
	}
	dir, _ := promptsDir()
	if len(written) == 0 {
		fmt.Printf("Prompts already exist at %s (use --force to overwrite).\n", dir)
		return nil
	}
	fmt.Printf("Wrote %d prompt template(s) to %s:\n", len(written), dir)
	for _, p := range written {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println("\nEdit these to customize each kind's behavior. `roster prompt show <kind>` renders one with placeholders substituted.")
	return nil
}

// cmdPrompt handles `roster prompt <show|edit> <kind>`.
func cmdPrompt(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roster prompt <show|edit> <kind>")
	}
	sub := args[0]
	switch sub {
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("usage: roster prompt show <kind> [--id X --parent Y --description ...]")
		}
		kind := args[1]
		if err := validateKind(kind); err != nil {
			return err
		}
		fs := flag.NewFlagSet("show", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		id := fs.String("id", "example-"+kind, "placeholder id")
		parent := fs.String("parent", "", "placeholder parent")
		desc := fs.String("description", "(example description)", "placeholder description")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		out, err := renderPrompt(kind, promptData{ID: *id, Parent: *parent, Description: *desc})
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	case "edit":
		if len(args) != 2 {
			return fmt.Errorf("usage: roster prompt edit <kind>")
		}
		kind := args[1]
		if err := validateKind(kind); err != nil {
			return err
		}
		path, _, err := ensurePromptOnDisk(kind, false)
		if err != nil {
			return err
		}
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		cmd := exec.Command(editor, path)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	default:
		return fmt.Errorf("prompt: unknown subcommand %q (want show|edit)", sub)
	}
}
