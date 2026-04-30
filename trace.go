package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// `roster trace <id>` — pretty-print an agent's recent JSONL turns.
//
// Replaces the python3 -c snippets I was writing during testing.
// One line per turn, colored, with tool calls indented under their
// assistant turn. --follow tails new turns as the agent works.

func cmdTrace(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tail := fs.Int("tail", 30, "show the last N turns (0 = all)")
	follow := fs.Bool("follow", false, "tail new turns as they arrive")
	fs.BoolVar(follow, "f", false, "alias for --follow")
	noColor := fs.Bool("no-color", false, "disable ANSI colors")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: roster trace <agent-id> [--tail N] [--follow]")
	}
	id := rest[0]

	path, err := traceJSONLPath(id)
	if err != nil {
		return err
	}

	c := newColors(!*noColor && isTTY(os.Stdout))

	turns, err := readTurns(path)
	if err != nil {
		return err
	}
	start := 0
	if *tail > 0 && len(turns) > *tail {
		start = len(turns) - *tail
	}
	for _, t := range turns[start:] {
		fmt.Print(formatTurn(t, c))
	}

	if !*follow {
		return nil
	}
	// Tail loop: re-read every 800ms, emit new turns. Cheap and works
	// across rotates because we re-stat each tick.
	seen := len(turns)
	for {
		time.Sleep(800 * time.Millisecond)
		more, err := readTurns(path)
		if err != nil {
			return err
		}
		for i := seen; i < len(more); i++ {
			fmt.Print(formatTurn(more[i], c))
		}
		seen = len(more)
	}
}

// traceJSONLPath finds the agent's current JSONL using the same logic
// fleetview uses (registered uuid first, fallback to newest in dir).
func traceJSONLPath(id string) (string, error) {
	a, err := loadAgent(id)
	if err != nil {
		return "", err
	}
	if a.SessionUUID == "" {
		return "", fmt.Errorf("agent %s has no session_uuid (stopped?)", id)
	}
	for _, base := range claudeProjectRoots() {
		matches, _ := filepath.Glob(filepath.Join(base, "*", a.SessionUUID+".jsonl"))
		if len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", fmt.Errorf("no JSONL found for session %s", a.SessionUUID)
}

func claudeProjectRoots() []string {
	var roots []string
	// Per-orch isolated dirs.
	if base, _ := rosterPath("ROSTER_DIR", "XDG_DATA_HOME", ".local/share", "claude"); base != "" {
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			if e.IsDir() {
				roots = append(roots, filepath.Join(base, e.Name(), "projects"))
			}
		}
	}
	// User's global ~/.claude.
	home, _ := os.UserHomeDir()
	roots = append(roots, filepath.Join(home, ".claude", "projects"))
	return roots
}

// turn is one rendered line in the trace output. Each JSONL entry
// produces one or more of these depending on shape (a user line with
// 3 tool_results yields 3 turns).
type turn struct {
	t     time.Time
	role  string // user | assistant | tool_use | tool_result | thinking | system
	tool  string // for tool_use/tool_result
	text  string // role text or tool input/output preview
	isErr bool
}

type jsonlRaw struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

func readTurns(path string) ([]turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var out []turn
	for scanner.Scan() {
		var raw jsonlRaw
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		switch raw.Type {
		case "user":
			out = append(out, parseTraceUser(raw)...)
		case "assistant":
			out = append(out, parseTraceAssistant(raw)...)
		}
	}
	return out, scanner.Err()
}

var fromTagRE = regexp.MustCompile(`(?ms)^<from\s+id="([^"]+)">\s*\n?(.*?)\n?</from>\s*$`)
var fromPrefixRE = regexp.MustCompile(`(?s)^\[from ([^\]]+)\]\n\n(.*)$`)
var suggestionsRE = regexp.MustCompile(`(?s)<suggestions>.*?</suggestions>\s*$`)

func parseTraceUser(raw jsonlRaw) []turn {
	// User line can be a bare string or {role, content}. Content can be
	// a string or an array of blocks (text + tool_result).
	var s string
	if err := json.Unmarshal(raw.Message, &s); err == nil && s != "" {
		return []turn{{t: raw.Timestamp, role: "user", text: cleanRelay(s)}}
	}
	var obj struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw.Message, &obj); err != nil {
		return nil
	}
	var asStr string
	if err := json.Unmarshal(obj.Content, &asStr); err == nil && asStr != "" {
		return []turn{{t: raw.Timestamp, role: "user", text: cleanRelay(asStr)}}
	}
	var blocks []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		ToolUseID string `json:"tool_use_id"`
		Content   any    `json:"content"`
		IsError   bool   `json:"is_error"`
	}
	if err := json.Unmarshal(obj.Content, &blocks); err != nil {
		return nil
	}
	var out []turn
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, turn{t: raw.Timestamp, role: "user", text: cleanRelay(b.Text)})
			}
		case "tool_result":
			out = append(out, turn{
				t:     raw.Timestamp,
				role:  "tool_result",
				text:  preview(toolResultText(b.Content), 160),
				isErr: b.IsError || looksLikeError(toolResultText(b.Content)),
			})
		}
	}
	return out
}

func parseTraceAssistant(raw jsonlRaw) []turn {
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw.Message, &msg); err != nil {
		return nil
	}
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	var out []turn
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text := suggestionsRE.ReplaceAllString(b.Text, "")
			text = strings.TrimSpace(text)
			if text != "" {
				out = append(out, turn{t: raw.Timestamp, role: "assistant", text: text})
			}
		case "thinking":
			// Ignore by default — too noisy.
		case "tool_use":
			out = append(out, turn{
				t:    raw.Timestamp,
				role: "tool_use",
				tool: b.Name,
				text: preview(toolInputSummary(b.Name, b.Input), 200),
			})
		}
	}
	return out
}

func toolResultText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprintf("%v", c)
}

func toolInputSummary(name string, input json.RawMessage) string {
	// Per-tool friendly summaries. Falls back to a JSON preview.
	var generic map[string]any
	if err := json.Unmarshal(input, &generic); err != nil {
		return string(input)
	}
	switch name {
	case "Bash":
		if cmd, ok := generic["command"].(string); ok {
			return cmd
		}
	case "Read":
		if p, ok := generic["file_path"].(string); ok {
			return p
		}
	case "Write":
		if p, ok := generic["file_path"].(string); ok {
			return p
		}
	case "Edit":
		if p, ok := generic["file_path"].(string); ok {
			return p
		}
	case "Grep":
		if p, ok := generic["pattern"].(string); ok {
			return p
		}
	case "WebSearch", "WebFetch":
		if q, ok := generic["query"].(string); ok {
			return q
		}
		if u, ok := generic["url"].(string); ok {
			return u
		}
	case "Agent":
		st, _ := generic["subagent_type"].(string)
		desc, _ := generic["description"].(string)
		if st != "" || desc != "" {
			return strings.TrimSpace(st + ": " + desc)
		}
	}
	b, _ := json.Marshal(generic)
	return string(b)
}

func cleanRelay(s string) string {
	if m := fromTagRE.FindStringSubmatch(s); m != nil {
		return "[from " + m[1] + "] " + strings.TrimSpace(m[2])
	}
	if m := fromPrefixRE.FindStringSubmatch(s); m != nil {
		return "[from " + m[1] + "] " + strings.TrimSpace(m[2])
	}
	return s
}

func preview(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ↵ ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

func looksLikeError(s string) bool {
	low := strings.ToLower(s)
	return strings.HasPrefix(low, "error") ||
		strings.Contains(low, "exit code 1") ||
		strings.Contains(low, "permission denied") ||
		strings.Contains(low, "no such file") ||
		strings.Contains(low, "read-only") ||
		strings.Contains(low, "failed:")
}

// --- formatting --------------------------------------------------------------

type colors struct {
	dim, user, asst, tool, result, errC, reset string
}

func newColors(useColor bool) colors {
	if !useColor {
		return colors{}
	}
	return colors{
		dim:    "\x1b[90m",
		user:   "\x1b[36m",  // cyan
		asst:   "\x1b[37m",  // white
		tool:   "\x1b[33m",  // yellow
		result: "\x1b[90m",  // gray
		errC:   "\x1b[31m",  // red
		reset:  "\x1b[0m",
	}
}

func (c colors) wrap(s, code string) string {
	if code == "" {
		return s
	}
	return code + s + c.reset
}

func formatTurn(t turn, c colors) string {
	ts := t.t.Local().Format("15:04:05")
	tsCol := c.wrap(ts, c.dim)
	switch t.role {
	case "user":
		return fmt.Sprintf("%s %s %s\n", tsCol, c.wrap("user      ", c.user), t.text)
	case "assistant":
		return fmt.Sprintf("%s %s %s\n", tsCol, c.wrap("assistant ", c.asst), t.text)
	case "tool_use":
		label := fmt.Sprintf("→ %-9s", t.tool)
		return fmt.Sprintf("%s %s %s\n", tsCol, c.wrap(label, c.tool), t.text)
	case "tool_result":
		col := c.result
		prefix := "← "
		if t.isErr {
			col = c.errC
			prefix = "✗ "
		}
		return fmt.Sprintf("%s %s %s\n", tsCol, c.wrap(prefix+"        ", col), c.wrap(t.text, col))
	}
	return ""
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
