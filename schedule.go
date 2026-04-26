package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CRUD on each orch's <CLAUDE_CONFIG_DIR>/scheduled_tasks.json. Claude
// Code reads the same file natively and runs the prompts when their
// cron matches; this CLI just lets you list / add / remove durable
// jobs from the command line, mirroring fleetview's HTTP surface.
//
// Storage shape (matches Claude Code + superbot3's broker):
//
//   { "tasks": [
//       { "id": "abc12345", "cron": "0 9 * * 1-5", "prompt": "...",
//         "createdAt": 1735689600000, "recurring": true,
//         "permanent": true }
//     ] }

type scheduleTask struct {
	ID        string `json:"id"`
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	CreatedAt int64  `json:"createdAt"`
	Recurring bool   `json:"recurring"`
	Permanent bool   `json:"permanent"`
}

type scheduleStore struct {
	Tasks []scheduleTask `json:"tasks"`
}

// schedulesPath returns <orch_claude_dir>/scheduled_tasks.json. Errors
// when the orch has no isolated dir (dispatcher / pre-isolation
// orchs); the user's global ~/.claude/scheduled_tasks.json is theirs
// to manage with `claude` directly.
func schedulesPath(orchID string) (string, error) {
	dir, err := orchestratorClaudeDir(orchID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "scheduled_tasks.json"), nil
}

func loadSchedules(orchID string) (string, scheduleStore, error) {
	path, err := schedulesPath(orchID)
	if err != nil {
		return "", scheduleStore{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, scheduleStore{Tasks: []scheduleTask{}}, nil
		}
		return path, scheduleStore{}, err
	}
	var s scheduleStore
	if err := json.Unmarshal(b, &s); err != nil {
		return path, scheduleStore{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Tasks == nil {
		s.Tasks = []scheduleTask{}
	}
	return path, s, nil
}

func saveSchedules(path string, s scheduleStore) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func newScheduleTaskID() string {
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	return hex.EncodeToString(rb[:])
}

// --- CLI dispatch ----------------------------------------------------------

func cmdSchedule(args []string) error {
	if len(args) < 1 {
		return scheduleUsage()
	}
	switch args[0] {
	case "list":
		return cmdScheduleList(args[1:])
	case "create", "add":
		return cmdScheduleCreate(args[1:])
	case "delete", "remove", "rm":
		return cmdScheduleDelete(args[1:])
	case "-h", "--help", "help":
		return scheduleUsage()
	default:
		return fmt.Errorf("unknown schedule subcommand %q\n%s", args[0], scheduleUsageText)
	}
}

const scheduleUsageText = `usage:
  roster schedule list <orch>
         List durable scheduled tasks for an orch.

  roster schedule create <orch> --cron "<expr>" --prompt "<text>" [--no-recurring]
         Add a task to <orch>'s scheduled_tasks.json. Recurring by default
         (matches Claude Code's CronCreate semantics).

  roster schedule delete <orch> <task-id>
         Remove a task by id. ids are 8 hex chars (see 'list').
`

func scheduleUsage() error {
	fmt.Print(scheduleUsageText)
	return nil
}

func cmdScheduleList(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roster schedule list <orch>")
	}
	orch := args[0]
	if _, err := loadAgent(orch); err != nil {
		return fmt.Errorf("schedule list: %w", err)
	}
	_, store, err := loadSchedules(orch)
	if err != nil {
		return fmt.Errorf("schedule list: %w", err)
	}
	if len(store.Tasks) == 0 {
		fmt.Fprintln(os.Stderr, "no schedules")
		return nil
	}
	tasks := append([]scheduleTask(nil), store.Tasks...)
	sort.SliceStable(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt > tasks[j].CreatedAt
	})
	for _, t := range tasks {
		recur := "once"
		if t.Recurring {
			recur = "recur"
		}
		prompt := strings.ReplaceAll(t.Prompt, "\n", " ")
		if len(prompt) > 60 {
			prompt = prompt[:57] + "…"
		}
		fmt.Printf("%-8s  %-20s  %-5s  %s\n", t.ID, t.Cron, recur, prompt)
	}
	return nil
}

func cmdScheduleCreate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: roster schedule create <orch> --cron \"<expr>\" --prompt \"<text>\" [--no-recurring]")
	}
	orch := args[0]
	fs := flag.NewFlagSet("schedule create", flag.ContinueOnError)
	cron := fs.String("cron", "", "cron expression, e.g. \"0 9 * * 1-5\"")
	prompt := fs.String("prompt", "", "prompt the orch will receive when the task fires")
	noRecurring := fs.Bool("no-recurring", false, "fire once at the next match instead of every match")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	*cron = strings.TrimSpace(*cron)
	*prompt = strings.TrimSpace(*prompt)
	if *cron == "" || *prompt == "" {
		return fmt.Errorf("--cron and --prompt are required")
	}
	if _, err := loadAgent(orch); err != nil {
		return fmt.Errorf("schedule create: %w", err)
	}
	path, store, err := loadSchedules(orch)
	if err != nil {
		return fmt.Errorf("schedule create: %w", err)
	}
	t := scheduleTask{
		ID:        newScheduleTaskID(),
		Cron:      *cron,
		Prompt:    *prompt,
		CreatedAt: time.Now().UnixMilli(),
		Recurring: !*noRecurring,
		Permanent: true,
	}
	store.Tasks = append(store.Tasks, t)
	if err := saveSchedules(path, store); err != nil {
		return fmt.Errorf("schedule create: %w", err)
	}
	fmt.Println(t.ID)
	fmt.Fprintf(os.Stderr, "added schedule %s (%s) → %s\n", t.ID, t.Cron, path)
	return nil
}

func cmdScheduleDelete(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: roster schedule delete <orch> <task-id>")
	}
	orch, taskID := args[0], args[1]
	if _, err := loadAgent(orch); err != nil {
		return fmt.Errorf("schedule delete: %w", err)
	}
	path, store, err := loadSchedules(orch)
	if err != nil {
		return fmt.Errorf("schedule delete: %w", err)
	}
	out := store.Tasks[:0]
	for _, t := range store.Tasks {
		if t.ID != taskID {
			out = append(out, t)
		}
	}
	if len(out) == len(store.Tasks) {
		return fmt.Errorf("schedule delete: no task with id %q", taskID)
	}
	store.Tasks = out
	if err := saveSchedules(path, store); err != nil {
		return fmt.Errorf("schedule delete: %w", err)
	}
	fmt.Fprintf(os.Stderr, "deleted %s\n", taskID)
	return nil
}
