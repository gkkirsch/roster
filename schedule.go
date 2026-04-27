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
	"strconv"
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
	case "update", "edit":
		return cmdScheduleUpdate(args[1:])
	case "-h", "--help", "help":
		return scheduleUsage()
	default:
		return fmt.Errorf("unknown schedule subcommand %q\n%s", args[0], scheduleUsageText)
	}
}

const scheduleUsageText = `usage:
  roster schedule list <orch>
         List durable scheduled tasks for an orch.

  roster schedule create <orch> --prompt "<text>" <when>
         Add a task to <orch>'s scheduled_tasks.json.

         <when> is one of:
           --cron "<expr>"           explicit 5-field cron expression
           --daily-at "11:00,15:00"  multiple times each day, comma-separated
           --once "2026-04-26T17:00" fire once at a specific local time
                                     (auto-deletes after firing)

         Recurring by default for --cron and --daily-at; --once forces
         recurring=false.

  roster schedule update <orch> <task-id> [--prompt "<text>"] [<when>]
         Replace prompt and/or schedule on an existing task.

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
		return fmt.Errorf("usage: roster schedule create <orch> --prompt \"<text>\" --cron|--daily-at|--once <value>")
	}
	orch := args[0]
	fs := flag.NewFlagSet("schedule create", flag.ContinueOnError)
	cron := fs.String("cron", "", "cron expression, e.g. \"0 9 * * 1-5\"")
	dailyAt := fs.String("daily-at", "", "comma-separated daily times, e.g. \"11:00,15:00,17:00\"")
	once := fs.String("once", "", "one-shot ISO local time, e.g. \"2026-04-26T17:00\"")
	prompt := fs.String("prompt", "", "prompt the orch will receive when the task fires")
	noRecurring := fs.Bool("no-recurring", false, "force recurring=false on --cron / --daily-at (--once is always one-shot)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	*prompt = strings.TrimSpace(*prompt)
	if *prompt == "" {
		return fmt.Errorf("--prompt is required")
	}

	cronExpr, recurring, err := resolveScheduleWhen(*cron, *dailyAt, *once, *noRecurring)
	if err != nil {
		return fmt.Errorf("schedule create: %w", err)
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
		Cron:      cronExpr,
		Prompt:    *prompt,
		CreatedAt: time.Now().UnixMilli(),
		Recurring: recurring,
		Permanent: true,
	}
	store.Tasks = append(store.Tasks, t)
	if err := saveSchedules(path, store); err != nil {
		return fmt.Errorf("schedule create: %w", err)
	}
	fmt.Println(t.ID)
	mode := "recurring"
	if !recurring {
		mode = "one-shot"
	}
	fmt.Fprintf(os.Stderr, "added schedule %s (%s, %s) → %s\n", t.ID, t.Cron, mode, path)
	return nil
}

// resolveScheduleWhen turns the three mutually-exclusive scheduling
// flags into a (cron, recurring) pair. Exactly one of cron / dailyAt
// / once must be set.
func resolveScheduleWhen(cron, dailyAt, once string, noRecurring bool) (string, bool, error) {
	cron = strings.TrimSpace(cron)
	dailyAt = strings.TrimSpace(dailyAt)
	once = strings.TrimSpace(once)
	set := 0
	for _, s := range []string{cron, dailyAt, once} {
		if s != "" {
			set++
		}
	}
	if set == 0 {
		return "", false, fmt.Errorf("one of --cron, --daily-at, --once is required")
	}
	if set > 1 {
		return "", false, fmt.Errorf("--cron, --daily-at, --once are mutually exclusive")
	}
	switch {
	case cron != "":
		return cron, !noRecurring, nil
	case dailyAt != "":
		expr, err := dailyAtToCron(dailyAt)
		if err != nil {
			return "", false, err
		}
		return expr, !noRecurring, nil
	default:
		expr, err := onceToCron(once)
		if err != nil {
			return "", false, err
		}
		// One-shot is always recurring=false. Claude Code auto-deletes
		// the task after it fires (see CronCreateTool.ts).
		return expr, false, nil
	}
}

// dailyAtToCron parses "11:00,15:00,17:00" into "0 11,15,17 * * *".
// Requires every time to share the same minute — cron's minute field
// is a single value when paired with a multi-hour list.
func dailyAtToCron(spec string) (string, error) {
	raw := strings.Split(spec, ",")
	hours := make([]int, 0, len(raw))
	minute := -1
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		hh, mm, err := parseHHMM(p)
		if err != nil {
			return "", fmt.Errorf("--daily-at %q: %w", p, err)
		}
		if minute == -1 {
			minute = mm
		} else if mm != minute {
			return "", fmt.Errorf("--daily-at: all times must share the same minute (got :%02d and :%02d)", minute, mm)
		}
		hours = append(hours, hh)
	}
	if len(hours) == 0 {
		return "", fmt.Errorf("--daily-at: no times provided")
	}
	parts := make([]string, len(hours))
	for i, h := range hours {
		parts[i] = strconv.Itoa(h)
	}
	return fmt.Sprintf("%d %s * * *", minute, strings.Join(parts, ",")), nil
}

// onceToCron parses an ISO-like local time ("2026-04-26T17:00" or
// "2026-04-26 17:00") into a date-locked cron ("0 17 26 4 *") that
// matches exactly once. Caller pairs this with recurring=false.
func onceToCron(spec string) (string, error) {
	layouts := []string{
		"2006-01-02T15:04",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02 15:04:05",
	}
	var t time.Time
	var err error
	for _, l := range layouts {
		t, err = time.ParseInLocation(l, spec, time.Local)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("--once %q: expected YYYY-MM-DDTHH:MM (local time)", spec)
	}
	if !t.After(time.Now()) {
		return "", fmt.Errorf("--once %q: time is in the past", spec)
	}
	return fmt.Sprintf("%d %d %d %d *", t.Minute(), t.Hour(), t.Day(), int(t.Month())), nil
}

func parseHHMM(s string) (hour, minute int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected HH:MM")
	}
	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("hour must be 0-23")
	}
	minute, err = strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("minute must be 0-59")
	}
	return hour, minute, nil
}

func cmdScheduleUpdate(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: roster schedule update <orch> <task-id> [--prompt \"<text>\"] [--cron|--daily-at|--once <value>]")
	}
	orch, taskID := args[0], args[1]
	fs := flag.NewFlagSet("schedule update", flag.ContinueOnError)
	cron := fs.String("cron", "", "cron expression")
	dailyAt := fs.String("daily-at", "", "comma-separated daily times")
	once := fs.String("once", "", "one-shot ISO local time")
	prompt := fs.String("prompt", "", "new prompt text")
	noRecurring := fs.Bool("no-recurring", false, "force recurring=false on --cron / --daily-at")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	*prompt = strings.TrimSpace(*prompt)

	if _, err := loadAgent(orch); err != nil {
		return fmt.Errorf("schedule update: %w", err)
	}
	path, store, err := loadSchedules(orch)
	if err != nil {
		return fmt.Errorf("schedule update: %w", err)
	}
	idx := -1
	for i, t := range store.Tasks {
		if t.ID == taskID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("schedule update: no task with id %q", taskID)
	}

	whenSet := strings.TrimSpace(*cron) != "" || strings.TrimSpace(*dailyAt) != "" || strings.TrimSpace(*once) != ""
	if whenSet {
		expr, recurring, err := resolveScheduleWhen(*cron, *dailyAt, *once, *noRecurring)
		if err != nil {
			return fmt.Errorf("schedule update: %w", err)
		}
		store.Tasks[idx].Cron = expr
		store.Tasks[idx].Recurring = recurring
	}
	if *prompt != "" {
		store.Tasks[idx].Prompt = *prompt
	}
	if !whenSet && *prompt == "" {
		return fmt.Errorf("schedule update: nothing to change (pass --prompt and/or --cron/--daily-at/--once)")
	}
	if err := saveSchedules(path, store); err != nil {
		return fmt.Errorf("schedule update: %w", err)
	}
	fmt.Fprintf(os.Stderr, "updated %s\n", taskID)
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
