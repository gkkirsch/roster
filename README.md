# roster

File-backed registry for Claude agents. Phone book + voicemail on top of
[`camux`](../camux). Tracks which agents you've spawned, their purpose
(as a rolling description), their lineage (parent → children), and
enough state to resume them across sessions.

```
roster spawn dispatch --kind dispatcher --description "routes user requests"
roster spawn orch-code --kind orchestrator --parent dispatch --description "owns ~/dev/amux"
roster spawn plan-01 --kind worker --parent orch-code --description "plans the auth refactor"

roster tree
# dispatch (dispatcher) [ready]  — routes user requests
# └── orch-code (orchestrator) [ready]  — owns ~/dev/amux
#     └── plan-01 (worker) [ready]  — plans the auth refactor

roster notify orch-code "please ask plan-01 for the auth plan" --from dispatch
# → appears as a new user turn in orch-code's TUI; orch-code's Claude
#   reads it and responds according to its system prompt
```

## Install

```
git clone ... ~/dev/roster
cd ~/dev/roster && make install
```

Requires `camux` and `amux` on `$PATH`.

## Commands

| Command | What it does |
|---|---|
| `spawn <id> [--kind K] [--parent P] [--description "…"] -- <camux-flags>` | Register + spawn via camux. Auto-injects a system-prompt appendix teaching the new Claude how to notify its parent. |
| `list [--kind K] [--parent P] [--status S] [--json]` | Enumerate registered agents with live status (via camux). |
| `tree` | ASCII lineage of all agents. |
| `describe <id> [--tail N] [--json]` | Full record + optional tail of the pane. |
| `target <id>` | Prints just the amux target (shell-friendly). |
| `search <query>` | Grep across descriptions and ids. |
| `update <id> [--description "…"] [--append "…"]` | Mutate the rolling description. |
| `resume <id>` | Re-spawn a stopped agent with `claude --resume <session_uuid>` + saved flags. Conversation continues. |
| `stop <id>` | Kill the tmux session but keep the record (resumable). |
| `forget <id>` | Delete the record entirely (and kill if live). |
| `notify <to-id> "<msg>" [--from <id>] [--wait-ready 30s]` | Wait until recipient is Ready, paste the message, submit. Delivered as a new user turn in their TUI. |

## Storage

Per-agent JSON at `~/.local/share/roster/agents/<id>.json`. Override with
`ROSTER_DIR`. One file per agent — concurrent writes don't conflict,
easy to `cat`/`jq`/`rm`, trivial backup via `tar`.

Fields (durable):
```json
{
  "id": "plan-01",
  "kind": "worker",
  "parent": "orch-code",
  "description": "plans the auth refactor. So far: identified 3 steps…",
  "session_uuid": "7aa1e88a-…",
  "spawn_args": ["--model", "sonnet", "--append-system", "…"],
  "cwd": "/repo",
  "target": "plan-01:cc",
  "created": "…",
  "last_seen": "…"
}
```

`status` is derived live via `camux status` on every query — registry
never lies about liveness.

## The messaging model

No polling. No queues. No JSONL bus. Every cross-agent message is:

```
roster notify <to-id> "<message>"
```

It waits (with timeout) until the recipient's TUI is `ready`, then
pastes + submits the message. The recipient's Claude sees it exactly as
if a user typed it.

This makes orchestration a chain of natural Claude conversations:

1. User (or dispatcher) notifies an orchestrator with a task.
2. Orchestrator reads it, decides what to do, potentially notifies a
   worker to do a subtask (via `roster notify worker "…"` from its
   Bash tool).
3. Worker does the work, calls `roster notify parent "done: …"`.
4. Parent's TUI gets the reply as a new user turn. Parent processes,
   decides next steps.

Every spawn with `--parent P` injects a system-prompt appendix
explaining the pattern to the new Claude, so this happens without any
orchestration logic inside roster itself. Roster is a registry, not an
engine.

## Proof

8 unit tests pass (validation, store roundtrip, lineage, system-prompt
helpers). End-to-end dogfood run:

- dispatcher spawned
- orchestrator spawned with `--parent dispatch`
- worker spawned with `--parent orch-code`
- external notify dispatch→orch; orch did work; orch auto-called
  `roster notify dispatch "..."` to report back
- full 4-hop relay: dispatch → orch → worker → orch → dispatch, all
  messages delivered as user turns, each Claude forwarded via its
  Bash tool
- stop + resume preserved the conversation (orch recalled its prior
  task after rehydration via session_uuid)
- forget killed + deleted the record
- search found agents by description

## What's out of scope

No plans, DAGs, schedulers, retries, cost tracking, or pub/sub. If you
want those, layer them on top of roster the same way roster layers on
camux. The registry stays small and obvious.
