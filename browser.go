package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Per-orchestrator browser isolation. One Chrome profile per orch, one
// CDP port per orch (deterministic from the orch ID), and a wrapper on
// PATH that forces every `agent-browser` invocation through that port.
// Workers inherit from their nearest orch ancestor — same shape as
// claudedir.go.

//go:embed assets/agent-browser
var agentBrowserWrapper []byte

const (
	// Range chosen to avoid Chrome's default 9222 and Vite/dev-server
	// ports. 100 slots → 100 simultaneous orchs before we collide.
	cdpPortBase  = 9300
	cdpPortRange = 100
)

// cdpPortFor returns the deterministic CDP port for an orch.
func cdpPortFor(orchID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(orchID))
	return cdpPortBase + int(h.Sum32()%uint32(cdpPortRange))
}

// browserProfileDir returns the per-orch Chrome user-data-dir, ensuring
// it exists.
func browserProfileDir(orchID string) (string, error) {
	base, err := rosterPath("ROSTER_DIR", "XDG_DATA_HOME", ".local/share", "browser-profiles")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, orchID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// browserBinDir is where the wrapper lives. Shared across orchs — the
// wrapper reads AGENT_BROWSER_CDP from env at run time.
func browserBinDir() (string, error) {
	return rosterPath("ROSTER_DIR", "XDG_DATA_HOME", ".local/share", "bin")
}

// installBrowserWrapper writes the embedded wrapper to <bin>/agent-browser
// AND symlinks it from ~/.local/bin/agent-browser so it lands on the
// user's normal PATH ahead of any global npm install. tmux
// set-environment doesn't reliably propagate PATH into new windows
// (default-shell zsh rebuilds PATH on startup), so the canonical
// install must already be somewhere zsh leaves on PATH.
//
// The wrapper falls through to the real agent-browser when
// AGENT_BROWSER_CDP is unset, so installing globally is safe — only
// roster orchs (where roster sets the env) get the enforced --cdp +
// blocked subcommands.
func installBrowserWrapper() (string, error) {
	canonical, err := installBrowserWrapperCanonical()
	if err != nil {
		return "", err
	}
	if err := ensureLocalBinSymlink("agent-browser", canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func installBrowserWrapperCanonical() (string, error) {
	dir, err := browserBinDir()
	if err != nil {
		return "", err
	}
	// Resolve the real agent-browser binary BEFORE we install our shim
	// — the discovery has to happen while the user's PATH still points
	// at the npm-installed (or other) original. Save it to a sidecar so
	// the wrapper can find it forever after.
	if err := writeRealAgentBrowserSidecar(dir); err != nil {
		// Non-fatal — the wrapper will surface a clear error if it
		// can't find the sidecar later.
		fmt.Fprintf(os.Stderr, "roster: agent-browser sidecar not written: %v\n", err)
	}
	path := filepath.Join(dir, "agent-browser")
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, agentBrowserWrapper) {
		_ = os.Chmod(path, 0o755)
		return path, nil
	}
	if err := os.WriteFile(path, agentBrowserWrapper, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// writeRealAgentBrowserSidecar resolves the real agent-browser binary
// path (skipping any symlink that already points at our shim) and
// writes it to <bin>/.agent-browser-real. The wrapper reads this at
// runtime to know where to exec — without it we'd risk recursing into
// our own symlink.
//
// Skipped if the sidecar already exists and points at an executable.
//
// Special case: asdf shims do dynamic PATH lookup at runtime. If asdf's
// .tool-versions says `system` (or our wrapper is first on PATH), the
// shim resolves back to our wrapper → infinite loop. So when the only
// candidate is an asdf shim, we parse the shim's `# asdf-plugin:`
// comment and resolve directly to ~/.asdf/installs/<plugin>/<ver>/bin/<cmd>.
func writeRealAgentBrowserSidecar(binDir string) error {
	sidecar := filepath.Join(binDir, ".agent-browser-real")
	if existing, err := os.ReadFile(sidecar); err == nil {
		path := strings.TrimSpace(string(existing))
		if fi, err := os.Stat(path); err == nil && fi.Mode()&0o111 != 0 && !isAsdfShim(path) {
			return nil
		}
	}
	ourShim := filepath.Join(binDir, "agent-browser")
	ourShimAbs, _ := filepath.Abs(ourShim)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || dir == binDir {
			continue
		}
		cand := filepath.Join(dir, "agent-browser")
		fi, err := os.Stat(cand)
		if err != nil || fi.Mode()&0o111 == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(cand)
		if err != nil {
			continue
		}
		resolvedAbs, _ := filepath.Abs(resolved)
		if resolvedAbs == ourShimAbs {
			continue
		}
		// asdf shims must be unwrapped — at runtime they go back through
		// PATH lookup, which finds our wrapper and recurses.
		if isAsdfShim(resolved) {
			real := unwrapAsdfShim(resolved)
			if real == "" {
				continue
			}
			resolved = real
		}
		return os.WriteFile(sidecar, []byte(resolved), 0o644)
	}
	return fmt.Errorf("no real agent-browser found on PATH (npm install -g agent-browser?)")
}

// isAsdfShim returns true if path looks like an asdf-generated shim
// (lives in an asdf shims dir or contains the marker comment).
func isAsdfShim(path string) bool {
	if strings.Contains(path, "/asdf/shims/") {
		return true
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(body), "asdf-plugin:") || strings.Contains(string(body), "asdf exec")
}

// unwrapAsdfShim parses an asdf shim's `# asdf-plugin: <plugin> <ver>`
// comment, constructs ~/.asdf/installs/<plugin>/<ver>/bin/<cmd>, and
// returns the EvalSymlinks-resolved path. Returns "" on failure.
//
// Tries every plugin/version combo listed in the shim (asdf shims with
// multiple managed versions list them all) and returns the first that
// resolves to an existing executable.
func unwrapAsdfShim(shimPath string) string {
	body, err := os.ReadFile(shimPath)
	if err != nil {
		return ""
	}
	cmd := filepath.Base(shimPath)
	home, _ := os.UserHomeDir()
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "# asdf-plugin:") {
			continue
		}
		parts := strings.Fields(strings.TrimPrefix(line, "# asdf-plugin:"))
		if len(parts) < 2 {
			continue
		}
		plugin, version := parts[0], parts[1]
		cand := filepath.Join(home, ".asdf", "installs", plugin, version, "bin", cmd)
		fi, err := os.Stat(cand)
		if err != nil || fi.Mode()&0o111 == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(cand); err == nil {
			return resolved
		}
		return cand
	}
	return ""
}

// browserOrchFor resolves which orch's browser context an agent should
// use. Orchestrators answer for themselves; workers walk up to their
// nearest orch; dispatchers (and orphan workers) get "" — meaning no
// browser env is provisioned.
func browserOrchFor(kind, id, parentID string) (string, error) {
	switch kind {
	case "orchestrator":
		return id, nil
	case "worker":
		return findOrchestratorAncestor(parentID)
	}
	return "", nil
}

// prepareBrowserIsolation does the per-spawn work: install the wrapper,
// compute the per-orch port + profile, and set env on the tmux session
// (CDP port, session name, profile dir, and a PATH that includes our
// wrapper bin dir). Safe to call for any agent — no-ops cleanly when
// the agent has no orch ancestor.
//
// Returns the resolved orch ID (empty if no isolation applied).
func prepareBrowserIsolation(kind, id, parentID, session string) (string, error) {
	orch, err := browserOrchFor(kind, id, parentID)
	if err != nil {
		return "", err
	}
	if orch == "" {
		return "", nil
	}
	bin, err := installBrowserWrapper()
	if err != nil {
		return "", err
	}
	profile, err := browserProfileDir(orch)
	if err != nil {
		return "", err
	}
	// Stamp <profile>/Default/Preferences with the orch's name + tint
	// before Chrome ever boots. Fleetview's globe button does the same
	// thing, but agents launch Chrome via the wrapper script which
	// doesn't — so without this, agent-launched Chrome shows up as
	// "Person 1" with the default theme.
	if err := writeProfileIdentity(profile, orch); err != nil {
		fmt.Fprintf(os.Stderr, "roster: writeProfileIdentity %s: %v\n", orch, err)
	}
	// prepareClaudeIsolation runs first and creates the session with the
	// right cwd; this is a defensive create-if-missing and the cwd arg
	// is only used when the session truly didn't exist yet.
	if err := ensureAmuxSession(session, agentSpaceDir(kind, id, parentID)); err != nil {
		return "", err
	}
	port := cdpPortFor(orch)
	binDir := filepath.Dir(bin)
	pathVal := binDir + ":" + os.Getenv("PATH")
	env := map[string]string{
		"AGENT_BROWSER_CDP":     strconv.Itoa(port),
		"AGENT_BROWSER_SESSION": orch,
		"AGENT_BROWSER_PROFILE": profile,
		"PATH":                  pathVal,
		// Daemons accumulate one-per-orch and persist forever. Auto-shut
		// after 30min idle so abandoned orchs don't leak processes.
		"AGENT_BROWSER_IDLE_TIMEOUT_MS": "1800000",
	}
	for k, v := range env {
		if err := setTmuxSessionEnv(session, k, v); err != nil {
			return "", err
		}
	}
	return orch, nil
}

// writeProfileIdentity sets the profile name (= orch ID) and a
// deterministic theme color in <profile>/Default/Preferences. Chrome
// reads this on first launch when there's no Secure Preferences yet,
// so a fresh profile picks up our name+tint and you can tell the
// windows apart at a glance. Mirrors fleetview's launcher — kept in
// sync so identity is written regardless of which path launches Chrome.
func writeProfileIdentity(profileDir, orchID string) error {
	defaultDir := filepath.Join(profileDir, "Default")
	if err := os.MkdirAll(defaultDir, 0o755); err != nil {
		return err
	}
	prefsPath := filepath.Join(defaultDir, "Preferences")
	prefs := map[string]any{}
	if b, err := os.ReadFile(prefsPath); err == nil {
		_ = json.Unmarshal(b, &prefs)
	}
	profile, _ := prefs["profile"].(map[string]any)
	if profile == nil {
		profile = map[string]any{}
	}
	profile["name"] = orchID
	profile["using_default_name"] = false
	profile["using_default_avatar"] = false
	profile["using_gaia_avatar"] = false
	prefs["profile"] = profile

	browser, _ := prefs["browser"].(map[string]any)
	if browser == nil {
		browser = map[string]any{}
	}
	theme, _ := browser["theme"].(map[string]any)
	if theme == nil {
		theme = map[string]any{}
	}
	theme["user_color"] = colorForOrch(orchID)
	browser["theme"] = theme
	prefs["browser"] = browser

	out, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(prefsPath, out, 0o644)
}

func colorForOrch(orchID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(orchID))
	hue := float64(h.Sum32()%360) / 360.0
	r, g, b := hslToRGB(hue, 0.55, 0.55)
	return (r << 16) | (g << 8) | b
}

func hslToRGB(h, s, l float64) (int, int, int) {
	if s == 0 {
		v := int(math.Round(l * 255))
		return v, v, v
	}
	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q
	r := hueToRGB(p, q, h+1.0/3)
	g := hueToRGB(p, q, h)
	b := hueToRGB(p, q, h-1.0/3)
	return int(math.Round(r * 255)), int(math.Round(g * 255)), int(math.Round(b * 255))
}

func hueToRGB(p, q, t float64) float64 {
	if t < 0 {
		t += 1
	}
	if t > 1 {
		t -= 1
	}
	if t < 1.0/6 {
		return p + (q-p)*6*t
	}
	if t < 1.0/2 {
		return q
	}
	if t < 2.0/3 {
		return p + (q-p)*(2.0/3-t)*6
	}
	return p
}

// chromeAlive probes the CDP /json/version endpoint with a short
// timeout. Anything other than a 2xx counts as not-alive.
func chromeAlive(port int) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	client := http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/json/version")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func chromeBinary() string {
	if v := os.Getenv("CHROME_BIN"); v != "" {
		return v
	}
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// launchChrome ensures Chrome is running for orchID with the per-orch
// profile + CDP port. No-op (returns the existing port/profile) if
// Chrome on that port is already alive. The flag-set is deliberately
// minimal so Chrome behaves like a normal user session — see fleetview's
// pre-consolidation comment for the reasoning around --enable-automation
// and --disable-blink-features.
func launchChrome(orchID string) (port int, profile string, err error) {
	port = cdpPortFor(orchID)
	profile, err = browserProfileDir(orchID)
	if err != nil {
		return port, "", err
	}
	if chromeAlive(port) {
		return port, profile, nil
	}
	if err := writeProfileIdentity(profile, orchID); err != nil {
		fmt.Fprintf(os.Stderr, "roster: writeProfileIdentity %s: %v\n", orchID, err)
	}
	chrome := chromeBinary()
	if chrome == "" {
		return port, profile, fmt.Errorf("Google Chrome not found on this system")
	}
	cmd := exec.Command(chrome,
		"--user-data-dir="+profile,
		"--remote-debugging-port="+strconv.Itoa(port),
		"--no-first-run",
		"--no-default-browser-check",
		"--window-size=1280,800",
		// Force DPR=1 so screenshots are reasonable bytes on retina
		// machines. agent-browser's `screenshot` reads at the
		// browser's native devicePixelRatio; without this flag a
		// MacBook Pro emits 2x captures that routinely blow past
		// model image-input limits and slow the orch's loop. See
		// vercel-labs/agent-browser#304.
		"--force-device-scale-factor=1",
		"about:blank",
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return port, profile, err
	}
	go func() { _ = cmd.Wait() }()
	for i := 0; i < 40; i++ {
		if chromeAlive(port) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	return port, profile, nil
}

// browserStatus is what `roster browser status|launch` prints — same
// shape fleetview's HTTP handler returns, so the UI can pass it through.
type browserStatus struct {
	OrchID  string `json:"orch_id"`
	Port    int    `json:"port"`
	Profile string `json:"profile"`
	Alive   bool   `json:"alive"`
	Error   string `json:"error,omitempty"`
}

// cmdBrowser implements `roster browser status|launch <orch>`. JSON-only
// output to stdout; meant for fleetview (and the agent-browser wrapper)
// to shell out to instead of duplicating the launch logic.
func cmdBrowser(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: roster browser status|launch <orch-id>")
	}
	sub, orch := args[0], args[1]
	if err := validateID(orch); err != nil {
		return err
	}
	port := cdpPortFor(orch)
	profile, err := browserProfileDir(orch)
	if err != nil {
		return err
	}
	s := browserStatus{OrchID: orch, Port: port, Profile: profile}
	switch sub {
	case "status":
		s.Alive = chromeAlive(port)
	case "launch":
		_, _, lerr := launchChrome(orch)
		s.Alive = chromeAlive(port)
		if lerr != nil {
			s.Error = lerr.Error()
		}
	default:
		return fmt.Errorf("unknown browser subcommand %q (want status|launch)", sub)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}
