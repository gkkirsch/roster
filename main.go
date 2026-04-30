package main

import (
	"fmt"
	"os"
)

const usageText = `roster — file-backed registry for Claude agents (phone book + voicemail).

Usage:
  roster <command> [flags] [args]

Lifecycle:
  spawn   <id> [--kind K] [--parent P] [--description "..."] -- <camux-spawn-flags>
          Register an agent, spawn the Claude via camux, print its target.
          --kind defaults to 'worker'. If --parent is given, a system-prompt
          appendix teaches the spawned Claude to notify its parent.
  resume  <id>
          Re-spawn a stopped agent using its saved session UUID + spawn_args.
  stop    <id>
          Kill the tmux session but keep the record (resumable).
  forget  <id>
          Delete the record (and kill if still running).

Visibility:
  list    [--kind K] [--parent P] [--status S] [--json]
  tree
  describe <id> [--tail N] [--json]
  target   <id>
  search  <query>

Collaboration:
  update  <id> [--description "..."] | [--append "..."]
  notify  <to-id> "<message>" [--from <id>] [--wait-ready 30s]
          Wait until recipient's TUI is ready, paste the message, submit.
          Appears in their Claude conversation as a new user turn.

          SHELL ESCAPING — read this. The message is a Bash arg, so Bash
          expands $vars, backticks, etc. BEFORE roster sees them. Common
          bug: "$19/mo" → "9/mo" because Bash treats $1 as positional.
          Three safe patterns:
            single quotes:   roster notify dispatch 'price is $19/mo'
            backslash:       roster notify dispatch "price is \$19/mo"
            heredoc:         roster notify dispatch --from X <<'EOF'
                             anything goes here, $vars and "quotes"
                             EOF
          When relaying user content verbatim, prefer the heredoc form.

Setup:
  init    [--force]
          Materialize the three default prompt templates into
          ~/.config/roster/prompts/{dispatcher,orchestrator,worker}.md.
          Run once; edit the templates to customize agent behavior.
  prompt show <kind> [--id X --parent Y --description "..."]
          Render a prompt template with placeholder values. Useful to
          see what an agent will actually see.
  prompt edit <kind>
          Open the template in $EDITOR.

Environment:
  CAMUX_BIN     overrides the camux executable name
  AMUX_BIN      overrides the amux executable name
  ROSTER_DIR    overrides the agents directory (default: ~/.local/share/roster/agents)
`

func main() {
	if v := os.Getenv("CAMUX_BIN"); v != "" {
		camuxBin = v
	}
	if v := os.Getenv("AMUX_BIN"); v != "" {
		amuxBin = v
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Print(usageText)
		return
	}
	var err error
	switch cmd {
	case "spawn":
		err = cmdSpawn(args)
	case "list", "ls":
		err = cmdList(args)
	case "tree":
		err = cmdTree(args)
	case "describe":
		err = cmdDescribe(args)
	case "target":
		err = cmdTarget(args)
	case "search":
		err = cmdSearch(args)
	case "update":
		err = cmdUpdate(args)
	case "resume":
		err = cmdResume(args)
	case "stop":
		err = cmdStop(args)
	case "forget":
		err = cmdForget(args)
	case "notify":
		err = cmdNotify(args)
	case "init":
		err = cmdInit(args)
	case "prompt":
		err = cmdPrompt(args)
	case "artifact":
		err = cmdArtifact(args)
	case "schedule", "schedules":
		err = cmdSchedule(args)
	default:
		fmt.Fprintf(os.Stderr, "roster: unknown command %q\n\n", cmd)
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "roster:", err)
		os.Exit(1)
	}
}
