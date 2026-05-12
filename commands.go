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
		return fmt.Errorf("spawn: agent %q already exists (use `roster ensure %s` or `roster forget %s` first)", id, id, id)
	}
	if *parent != "" && !agentExists(*parent) {
		return fmt.Errorf("spawn: --parent %q not in roster", *parent)
	}

	// Resolve the orch's two directories now so the system prompt
	// can reference them as literal absolute paths — no $env_var
	// expansion required at runtime. claudeDirFor returns "" for
	// kinds that share their parent's claude dir (workers); the
	// template handles "" gracefully.
	claudeDir, err := claudeDirFor(*kind, id, *parent)
	if err != nil {
		return fmt.Errorf("spawn: resolve claude dir: %w", err)
	}
	systemPrompt, err := renderPrompt(*kind, promptData{
		ID:          id,
		Parent:      *parent,
		Description: *description,
		Space:       agentSpaceDir(*kind, id, *parent),
		ClaudeDir:   claudeDir,
	})
	if err != nil {
		return fmt.Errorf("spawn: render system prompt: %w", err)
	}

	// Persist the rendered prompt to disk and pass it to claude via
	// --system-prompt-file rather than the inline --system-prompt. The
	// inline form gets mangled in transit: tmux's `new-window -- cmd
	// args` runs the command through the user's default shell, which
	// expands `$VARS`, backticks, and other metacharacters in the
	// prompt content. Our prompts are markdown that intentionally
	// contains shell-escaping examples ("the price is $19/mo"), so
	// inline passing produced corrupted prompts inside claude. Path
	// passing avoids the shell entirely — claude reads the file
	// directly via system calls.
	promptPath, err := writeSpawnPrompt(id, systemPrompt)
	if err != nil {
		return fmt.Errorf("spawn: write system-prompt file: %w", err)
	}

	// Prepend --system-prompt-file so any user-provided one later in
	// camuxFlags wins (claude's CLI: later flag overrides earlier).
	baseFlags := []string{"--system-prompt-file", promptPath}
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
	// Pin claude's $PWD to the agent's space (per-orch dir under
	// ~/Library/Application Support/Director) so all $PWD-relative
	// writes land where workers + the user expect.
	spawnArgs = append(spawnArgs, "--cwd", agentSpaceDir(*kind, id, *parent))

	out, err := runCamux(spawnArgs...)
	if err != nil {
		return fmt.Errorf("spawn: camux spawn failed: %w", err)
	}
	target := strings.TrimSpace(strings.Split(out, "\n")[0])
	if target == "" {
		return fmt.Errorf("spawn: camux spawn returned empty target, stdout=%q", out)
	}

	// Pull the Claude session UUID via camux info (best-effort — if it
	// fails, we still save the record without it; ensure can't restore
	// the conversation but the record is still valid).
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

// migrateLegacySystemPromptArgs rewrites `--system-prompt <inline>`
// pairs in saved SpawnArgs to `--system-prompt-file <path>` by
// writing the inline content to a stable file via writeSpawnPrompt.
// Idempotent: no-op when the args already use --system-prompt-file.
// On any write error, returns the args unchanged + rewrote=false so
// the caller falls back to the original (still-broken-but-safe)
// path. We swallow the error rather than failing the whole resume
// because (a) writeSpawnPrompt failures are rare and (b) the worst
// outcome of the fallback is the same legacy crash the user already
// has — we don't want to make resume LESS likely to succeed.
func migrateLegacySystemPromptArgs(id string, args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	rewrote := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--system-prompt" && i+1 < len(args) {
			path, err := writeSpawnPrompt(id, args[i+1])
			if err != nil {
				// Bail on rewrite, keep the rest of args untouched.
				out = append(out, args[i:]...)
				return out, false
			}
			out = append(out, "--system-prompt-file", path)
			i++ // skip the inline content
			rewrote = true
			continue
		}
		out = append(out, args[i])
	}
	return out, rewrote
}

// --- ensure -----------------------------------------------------------------

// cmdEnsure makes a registered agent's tmux target live. Idempotent:
//
//   - Already alive (tmux session up)  → print target, done. Do NOT respawn.
//   - Stopped / dead / window missing  → tear down any stale tmux session
//                                        defensively, then camux spawn,
//                                        persist the (possibly new) target.
//   - Saved SessionUUID won't restore  → blow it away once and retry fresh,
//                                        so a missing JSONL doesn't strand
//                                        the orch.
//
// The "alive" gate is critical for cross-relaunch survival: the dispatcher's
// tmux session persists across app quits (intentional — preserves the
// conversation). On relaunch, ensure must adopt the orphan, not respawn
// over it. amux exists is the authoritative liveness signal — it ignores
// camux's status string (which can transiently report "stopped" while
// claude is booting, scrolled, or showing a banner) and only asks tmux
// directly. Without this gate, a non-"ready" status causes a respawn,
// which collides with the alive claude on the JSONL lock; the new claude
// crashes before tmux can render remain-on-exit, and director-app's setup
// page loops forever on "window <id>:cc vanished after creation".
//
// All callers that want "this agent should be running" use this. Spawn
// is reserved for first-time registration; ensure is for everything else.
func cmdEnsure(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: roster ensure <id>")
	}
	id := args[0]
	a, err := loadAgent(id)
	if err != nil {
		return err
	}
	if a.Target != "" {
		sess := strings.SplitN(a.Target, ":", 2)[0]
		if camuxStatus(a.Target) == "ready" || amuxSessionExists(sess) {
			a.LastSeen = time.Now().UTC()
			_ = saveAgent(a)
			fmt.Println(a.Target)
			return nil
		}
	}

	// Defensive cleanup: if a tmux session under our id somehow exists
	// (orphan from a prior crash, leftover from a botched ensure that
	// got past the alive-gate but lost its target on disk), tear it
	// down before spawning. Otherwise camux spawn will create a new
	// window in the existing session and claude will race the JSONL
	// lock with whatever's inside.
	_, _ = runAmux("kill", a.ID)

	session := a.ID
	if _, err := prepareClaudeIsolation(a.Kind, a.ID, a.Parent, session); err != nil {
		return fmt.Errorf("ensure: claude dir isolation: %w", err)
	}
	if _, err := prepareBrowserIsolation(a.Kind, a.ID, a.Parent, session); err != nil {
		return fmt.Errorf("ensure: browser env: %w", err)
	}

	// Migrate legacy --system-prompt <inline> args to --system-prompt-file
	// <path>. Inline prompts get mangled passing through tmux's shell, and
	// claude crashes on launch. Rewrite once and persist.
	if migrated, rewrote := migrateLegacySystemPromptArgs(a.ID, a.SpawnArgs); rewrote {
		a.SpawnArgs = migrated
		if err := saveAgent(a); err != nil {
			return fmt.Errorf("ensure: persist migrated spawn args: %w", err)
		}
	}

	out, err := runCamuxSpawn(a)
	if err != nil && a.SessionUUID != "" {
		// `claude --resume <uuid>` failed — JSONL was deleted/rolled, or
		// the UUID was stale. Forget it and try fresh. We retry exactly
		// once: if a fresh spawn also fails, that's the real underlying
		// problem; surface it directly rather than chaining error noise.
		a.SessionUUID = ""
		out, err = runCamuxSpawn(a)
	}
	if err != nil {
		return fmt.Errorf("ensure: %w", err)
	}

	target := strings.TrimSpace(strings.Split(out, "\n")[0])
	a.Target = target
	a.LastSeen = time.Now().UTC()
	// Pull the live claude session UUID via camux info. Best-effort: if
	// camux can't read it, the record is still valid — we just won't be
	// able to --resume on the next ensure call (a fresh spawn will run).
	if info, err := camuxInfo(target); err == nil && info.SessionID != "" {
		a.SessionUUID = info.SessionID
	}
	if err := saveAgent(a); err != nil {
		return err
	}
	fmt.Println(target)
	return nil
}

// runCamuxSpawn invokes `camux spawn` with the agent's saved flags. Adds
// --resume only when SessionUUID is set; pins --cwd to the agent's space.
func runCamuxSpawn(a *Agent) (string, error) {
	flags := append([]string{}, a.SpawnArgs...)
	if a.SessionUUID != "" {
		flags = append(flags, "--resume", a.SessionUUID)
	}
	flags = append(flags, "--cwd", agentSpaceDir(a.Kind, a.ID, a.Parent))
	return runCamux(append([]string{"spawn", a.ID}, flags...)...)
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
	usage := "usage: roster notify <to-id> \"<message>\" [--from <id>]\n" +
		"  Bash expands $vars and backticks BEFORE roster sees the message —\n" +
		"  '$19/mo' becomes '9/mo'. Use single quotes 'price is $19', backslash\n" +
		"  \"\\$19\", or a heredoc <<'EOF'…EOF when the message has special chars."
	// Pull <to-id> and <message> out as the first two non-flag args so users
	// can write `notify --from X <to> "<msg>"` OR `notify <to> "<msg>" --from X`.
	// Go's flag.Parse stops at the first non-flag, which would force one
	// ordering otherwise.
	flagsWithValue := map[string]bool{
		"--from": true, "-from": true,
	}
	var positional []string
	var passthrough []string
	skipNext := false
	for _, a := range args {
		if skipNext {
			passthrough = append(passthrough, a)
			skipNext = false
			continue
		}
		if !strings.HasPrefix(a, "-") && len(positional) < 2 {
			positional = append(positional, a)
			continue
		}
		passthrough = append(passthrough, a)
		if flagsWithValue[a] {
			skipNext = true
		}
	}
	if len(positional) < 1 {
		return fmt.Errorf("%s", usage)
	}
	to := positional[0]
	var msg string
	switch {
	case len(positional) >= 2:
		msg = positional[1]
	case !isTTY(os.Stdin):
		// Heredoc / pipe form: `roster notify dispatch <<'EOF'…EOF`. Reading
		// from stdin sidesteps Bash $-expansion entirely, which is the safe
		// option `roster help` advertises for messages with shell metachars.
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("notify: read stdin: %w", err)
		}
		msg = strings.TrimRight(string(body), "\n")
		if msg == "" {
			return fmt.Errorf("notify: stdin was empty\n%s", usage)
		}
	default:
		return fmt.Errorf("%s", usage)
	}
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "id of the sender (optional; prepended to the delivered message)")
	if err := fs.Parse(passthrough); err != nil {
		return err
	}
	a, err := loadAgent(to)
	if err != nil {
		return err
	}

	// Self-heal: if the agent's tmux target is offline, ensure brings it
	// back from saved spawn args before we try to deliver. The dispatcher
	// routinely shells `roster notify <orch>` for orchs that may have been
	// reaped after a reboot or claude exit. director-server's HTTP /notify
	// has the same self-heal — both paths now behave consistently.
	offline := func(s string) bool { return s == "not-found" || s == "dead" || s == "stopped" }
	if a.Target == "" || offline(camuxStatus(a.Target)) {
		fmt.Fprintf(os.Stderr, "roster: notify: %s is offline — ensuring\n", to)
		if err := cmdEnsure([]string{to}); err != nil {
			return fmt.Errorf("notify: ensure of %s failed: %w", to, err)
		}
		a, err = loadAgent(to)
		if err != nil {
			return err
		}
	}
	if a.Target == "" {
		return fmt.Errorf("notify: %s has no target after ensure (saved spawn args missing?)", to)
	}
	if err := preflightNotify(a.Target); err != nil {
		return fmt.Errorf("notify: %w", err)
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
			// Mirrors the "do not reply to acknowledgments" rule in the
			// orchestrator/worker prompts: replying is opt-in, not
			// reflexive. Without this softening, recipients read the
			// "to respond, do X" footer as an instruction and ack-of-ack
			// loops are the result.
			footer = fmt.Sprintf(
				"\n\nReply only if you have substance for %s — a question, a blocker, or a result they need. Routine acknowledgments are noise; silence is the right answer when there's nothing to add. When you do reply: `roster notify %s \"<your reply>\" --from <your-agent-id>` (plain text does NOT reach %s).",
				*from, *from, *from,
			)
		}
		delivered = fmt.Sprintf("<from id=\"%s\">\n%s%s\n</from>", *from, msg, footer)
	}
	// Paste + submit directly via amux. camux `ask` would block waiting
	// for a reply, which isn't what notify semantics imply (fire-and-forget
	// arrival). claude's TUI buffers input even mid-stream, so this lands
	// either as the current turn (if the agent was at the prompt) or
	// queued for the next turn (if the agent was streaming).
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
		// `roster prompt show` is for inspection. Fill Space + ClaudeDir
		// with what real spawns would resolve them to, so the rendered
		// preview matches what an orch would actually see.
		claudeDir, _ := claudeDirFor(kind, *id, *parent)
		out, err := renderPrompt(kind, promptData{
			ID:          *id,
			Parent:      *parent,
			Description: *desc,
			Space:       agentSpaceDir(kind, *id, *parent),
			ClaudeDir:   claudeDir,
		})
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
	case "refresh":
		// Re-render an agent's per-spawn prompt file from the current
		// template. Per-spawn prompts are written once at spawn time and
		// passed to claude via --system-prompt-file; without this command,
		// edits to ~/.config/roster/prompts/<kind>.md only affect future
		// spawns. Refresh + stop + ensure re-applies the template to a
		// running agent.
		fs := flag.NewFlagSet("refresh", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		all := fs.Bool("all", false, "refresh every registered agent")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rest := fs.Args()
		var ids []string
		switch {
		case *all && len(rest) == 0:
			agents, err := listAgents()
			if err != nil {
				return err
			}
			for _, a := range agents {
				ids = append(ids, a.ID)
			}
		case !*all && len(rest) == 1:
			ids = []string{rest[0]}
		default:
			return fmt.Errorf("usage: roster prompt refresh <id> | roster prompt refresh --all")
		}
		for _, id := range ids {
			a, err := loadAgent(id)
			if err != nil {
				return fmt.Errorf("refresh %s: %w", id, err)
			}
			claudeDir, err := claudeDirFor(a.Kind, a.ID, a.Parent)
			if err != nil {
				return fmt.Errorf("refresh %s: resolve claude dir: %w", id, err)
			}
			rendered, err := renderPrompt(a.Kind, promptData{
				ID:          a.ID,
				Parent:      a.Parent,
				Description: a.Description,
				Space:       agentSpaceDir(a.Kind, a.ID, a.Parent),
				ClaudeDir:   claudeDir,
			})
			if err != nil {
				return fmt.Errorf("refresh %s: render: %w", id, err)
			}
			path, err := writeSpawnPrompt(a.ID, rendered)
			if err != nil {
				return fmt.Errorf("refresh %s: write: %w", id, err)
			}
			fmt.Printf("✓ %s → %s\n", id, path)
		}
		fmt.Fprintln(os.Stderr, "\nClaude reads --system-prompt-file at launch only — `roster stop <id> && roster ensure <id>` to apply.")
		return nil
	default:
		return fmt.Errorf("prompt: unknown subcommand %q (want show|edit|refresh)", sub)
	}
}

// --- health -----------------------------------------------------------------

// healthReport mirrors the JSON we emit from `roster health <id>`.
// director-server's /api/health/dispatcher proxies this report to the
// setup page; the setup page uses .Healthy as the gate to leave.
type healthReport struct {
	ID                string `json:"id"`
	Target            string `json:"target"`
	TmuxSessionExists bool   `json:"tmux_session_exists"`
	CamuxStatus       string `json:"camux_status"`
	SessionUUID       string `json:"session_uuid,omitempty"`
	Healthy           bool   `json:"healthy"`
	Reason            string `json:"reason,omitempty"`
}

// cmdHealth reports on an agent's actual on-disk + tmux + camux state
// as JSON. Same liveness logic as cmdEnsure's alive-gate: tmux session
// existing under the agent's id is sufficient evidence the dispatcher
// is reachable, even if claude inside is mid-stream or showing a banner.
//
// Exists so director-server can answer "is the dispatcher OK?" without
// kicking another `ensure` (which writes state and could race a running
// init goroutine). Pure read.
func cmdHealth(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: roster health <id>")
	}
	id := args[0]
	a, err := loadAgent(id)
	if err != nil {
		return err
	}
	h := healthReport{
		ID:          a.ID,
		Target:      a.Target,
		SessionUUID: a.SessionUUID,
	}
	var sess string
	if a.Target != "" {
		sess = strings.SplitN(a.Target, ":", 2)[0]
		h.TmuxSessionExists = amuxSessionExists(sess)
		h.CamuxStatus = camuxStatus(a.Target)
	}
	switch {
	case a.Target == "":
		h.Reason = "no target recorded — needs spawn"
	case !h.TmuxSessionExists:
		h.Reason = "tmux session " + sess + " not found"
	case h.CamuxStatus == "dead":
		h.Reason = "claude pane is dead"
	default:
		h.Healthy = true
	}
	b, _ := json.MarshalIndent(h, "", "  ")
	fmt.Println(string(b))
	return nil
}
