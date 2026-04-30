package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// `roster doctor` — surface common config issues that make agents fail.
//
// Workers fail in mysterious ways: agent-browser daemon stuck on a dead
// Chrome, sidecar pointing at a shim that recurses, claude-code missing
// from PATH, tmux server down, browser profiles for forgotten orchs
// piling up. Each requires different spelunking. This command rolls them
// up into one report.
//
// Output uses ✓ ⚠ ✗ markers — green/yellow/red is overkill for what's
// usually a 10-line report.

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fix := fs.Bool("fix", false, "clean up safe stuff: stale daemons (>1d old), browser profiles for forgotten orchs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	r := newReport()
	checkAgentBrowser(r, *fix)
	checkClaude(r)
	checkTmux(r)
	checkBrowserProfiles(r, *fix)
	checkChromeOrphans(r, *fix)

	r.print(os.Stdout)
	if r.fail > 0 {
		return fmt.Errorf("%d failed check(s)", r.fail)
	}
	return nil
}

type report struct {
	lines []reportLine
	fail  int
}

type reportLine struct {
	level  rune // ✓ ⚠ ✗
	title  string
	detail string
}

func newReport() *report { return &report{} }

func (r *report) ok(title, detail string)   { r.add('✓', title, detail) }
func (r *report) warn(title, detail string) { r.add('⚠', title, detail) }
func (r *report) bad(title, detail string)  { r.fail++; r.add('✗', title, detail) }

func (r *report) add(level rune, title, detail string) {
	r.lines = append(r.lines, reportLine{level, title, detail})
}

func (r *report) print(w io.Writer) {
	for _, l := range r.lines {
		fmt.Fprintf(w, "%c  %s\n", l.level, l.title)
		if l.detail != "" {
			for _, line := range strings.Split(strings.TrimRight(l.detail, "\n"), "\n") {
				fmt.Fprintf(w, "   %s\n", line)
			}
		}
	}
}

// --- agent-browser checks ---------------------------------------------------

func checkAgentBrowser(r *report, fix bool) {
	binDir, err := browserBinDir()
	if err != nil {
		r.bad("agent-browser bin dir", err.Error())
		return
	}
	wrapper := filepath.Join(binDir, "agent-browser")
	if fi, err := os.Stat(wrapper); err != nil || fi.Mode()&0o111 == 0 {
		r.bad("agent-browser wrapper", fmt.Sprintf("missing or non-executable at %s — run a `roster spawn` to install", wrapper))
	} else {
		r.ok("agent-browser wrapper", wrapper)
	}

	checkAgentBrowserSymlink(r, wrapper)
	checkAgentBrowserSidecar(r, binDir)
	checkAgentBrowserDaemons(r, fix)
}

func checkAgentBrowserSymlink(r *report, wrapper string) {
	home, _ := os.UserHomeDir()
	sym := filepath.Join(home, ".local", "bin", "agent-browser")
	target, err := os.Readlink(sym)
	if err != nil {
		r.warn("~/.local/bin/agent-browser symlink", "missing — orchs spawned via Flow.app will use the wrapper, but plain `agent-browser` from your shell will not be intercepted")
		return
	}
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(filepath.Dir(sym), target)
	}
	wrapperAbs, _ := filepath.Abs(wrapper)
	absResolved, _ := filepath.Abs(abs)
	if absResolved != wrapperAbs {
		r.warn("~/.local/bin/agent-browser symlink", fmt.Sprintf("points at %s, expected %s", abs, wrapper))
		return
	}
	r.ok("~/.local/bin/agent-browser symlink", "→ "+wrapper)
}

func checkAgentBrowserSidecar(r *report, binDir string) {
	sidecar := filepath.Join(binDir, ".agent-browser-real")
	body, err := os.ReadFile(sidecar)
	if err != nil {
		r.warn("agent-browser sidecar", "absent — will be recreated on next spawn")
		return
	}
	target := strings.TrimSpace(string(body))
	fi, err := os.Stat(target)
	if err != nil || fi.Mode()&0o111 == 0 {
		r.bad("agent-browser sidecar", fmt.Sprintf("points at %q which is missing or non-executable", target))
		return
	}
	if isAsdfShim(target) {
		r.bad("agent-browser sidecar", fmt.Sprintf("points at asdf shim %q — wrapper will recurse forever. Delete %s and re-spawn.", target, sidecar))
		return
	}
	r.ok("agent-browser sidecar", "→ "+target)
}

func checkAgentBrowserDaemons(r *report, fix bool) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".agent-browser")
	entries, err := os.ReadDir(dir)
	if err != nil {
		r.ok("agent-browser daemons", "no state dir (no daemons ever spawned)")
		return
	}
	type daemon struct {
		name  string
		pid   int
		alive bool
		age   time.Duration
	}
	var daemons []daemon
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".pid")
		pidPath := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(pidPath)
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		fi, _ := os.Stat(pidPath)
		alive := pid > 0 && processAlive(pid)
		age := time.Duration(0)
		if fi != nil {
			age = time.Since(fi.ModTime())
		}
		daemons = append(daemons, daemon{name: name, pid: pid, alive: alive, age: age})
	}
	sort.Slice(daemons, func(i, j int) bool { return daemons[i].name < daemons[j].name })
	if len(daemons) == 0 {
		r.ok("agent-browser daemons", "none active")
		return
	}
	var stale, orphan []daemon
	var detail strings.Builder
	for _, d := range daemons {
		mark := "alive"
		if !d.alive {
			mark = "ORPHAN (pid dead)"
			orphan = append(orphan, d)
		} else if d.age > 24*time.Hour {
			mark = fmt.Sprintf("stale (%s old)", roundAge(d.age))
			stale = append(stale, d)
		}
		fmt.Fprintf(&detail, "%-30s pid=%-7d %s\n", d.name, d.pid, mark)
	}
	level := "ok"
	if len(orphan) > 0 || len(stale) > 0 {
		level = "warn"
	}
	switch level {
	case "warn":
		r.warn(fmt.Sprintf("agent-browser daemons (%d total, %d orphan, %d stale)", len(daemons), len(orphan), len(stale)), strings.TrimRight(detail.String(), "\n"))
	default:
		r.ok(fmt.Sprintf("agent-browser daemons (%d total)", len(daemons)), strings.TrimRight(detail.String(), "\n"))
	}
	if fix && (len(orphan) > 0 || len(stale) > 0) {
		killed := 0
		for _, d := range append(orphan, stale...) {
			if d.alive {
				_ = killProcess(d.pid)
			}
			for _, ext := range []string{".pid", ".sock", ".stream", ".version"} {
				_ = os.Remove(filepath.Join(dir, d.name+ext))
			}
			killed++
		}
		r.ok("--fix", fmt.Sprintf("removed %d daemon(s)", killed))
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(nil) == nil
}

func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(os.Interrupt)
}

func roundAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// --- claude / tmux ----------------------------------------------------------

func checkClaude(r *report) {
	path, err := exec.LookPath("claude")
	if err != nil {
		r.bad("claude binary", "not on PATH — workers will fail to spawn")
		return
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		r.bad("claude binary", fmt.Sprintf("%s exists but --version failed: %v", path, err))
		return
	}
	r.ok("claude binary", strings.TrimSpace(string(out))+"  ("+path+")")
}

func checkTmux(r *report) {
	if _, err := exec.LookPath("tmux"); err != nil {
		r.bad("tmux binary", "not on PATH — required by camux")
		return
	}
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").CombinedOutput()
	if err != nil {
		// list-sessions exits non-zero when no server is running. That's
		// fine — server starts on first new-session. Just note it.
		r.ok("tmux", "server not running (will start on first spawn)")
		return
	}
	sessions := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(sessions) == 1 && sessions[0] == "" {
		r.ok("tmux", "server up, no sessions")
		return
	}
	r.ok("tmux", fmt.Sprintf("server up, %d session(s): %s", len(sessions), strings.Join(sessions, ", ")))
}

// --- browser profiles & Chrome orphans --------------------------------------

func checkBrowserProfiles(r *report, fix bool) {
	base, err := rosterPath("ROSTER_DIR", "XDG_DATA_HOME", ".local/share", "browser-profiles")
	if err != nil {
		r.warn("browser profiles", err.Error())
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		r.ok("browser profiles", "no profiles dir")
		return
	}
	known := rosterAgentIDs()
	var stale []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !known[e.Name()] {
			stale = append(stale, e.Name())
		}
	}
	if len(stale) == 0 {
		r.ok(fmt.Sprintf("browser profiles (%d)", len(entries)), base)
		return
	}
	r.warn(fmt.Sprintf("browser profiles (%d total, %d for forgotten orchs)", len(entries), len(stale)), strings.Join(stale, "\n"))
	if fix {
		removed := 0
		for _, name := range stale {
			if err := os.RemoveAll(filepath.Join(base, name)); err == nil {
				removed++
			}
		}
		r.ok("--fix", fmt.Sprintf("removed %d stale browser profile(s)", removed))
	}
}

// rosterAgentIDs returns IDs of every agent in the roster — reused for
// the browser-profile staleness check.
func rosterAgentIDs() map[string]bool {
	agents, err := listAgents()
	if err != nil {
		return nil
	}
	known := make(map[string]bool, len(agents))
	for _, a := range agents {
		known[a.ID] = true
	}
	return known
}

func checkChromeOrphans(r *report, fix bool) {
	out, err := exec.Command("ps", "-Ao", "pid=,command=").CombinedOutput()
	if err != nil {
		r.warn("Chrome orphan check", err.Error())
		return
	}
	var orphans []int
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "agent-browser-chrome-") {
			continue
		}
		// Parent process to look at: we want the top-level Chrome (the
		// one with --user-data-dir=/tmp/agent-browser-chrome-<uuid>) NOT
		// every helper. Filter by Google Chrome (no Helper).
		if strings.Contains(line, "Google Chrome Helper") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		orphans = append(orphans, pid)
	}
	if len(orphans) == 0 {
		r.ok("Chrome orphans", "none")
		return
	}
	pidStrs := make([]string, len(orphans))
	for i, p := range orphans {
		pidStrs[i] = strconv.Itoa(p)
	}
	r.warn(fmt.Sprintf("Chrome orphans (%d)", len(orphans)), "agent-browser auto-spawned these (port=0, headless). PIDs: "+strings.Join(pidStrs, ", "))
	if fix {
		killed := 0
		for _, pid := range orphans {
			if err := killProcess(pid); err == nil {
				killed++
			}
		}
		r.ok("--fix", fmt.Sprintf("killed %d orphan Chrome process(es)", killed))
	}
}
