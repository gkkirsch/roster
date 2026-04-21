package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// camuxBin — overridable via CAMUX_BIN env.
var camuxBin = "camux"
var amuxBin = "amux"

func runCamux(args ...string) (string, error) {
	return runCamuxStdin(nil, args...)
}

func runCamuxStdin(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command(camuxBin, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("camux %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

func runAmux(args ...string) (string, error) {
	cmd := exec.Command(amuxBin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("amux %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// camuxStatus returns camux's state string, or "not-found" / "error" if the
// target doesn't resolve. Purely derived, never written to disk.
func camuxStatus(target string) string {
	if target == "" {
		return "stopped"
	}
	cmd := exec.Command(camuxBin, "status", target)
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run() // exit code varies; we read stdout
	return strings.TrimSpace(out.String())
}

// camuxInfo invokes `camux info <target> --json` and parses it.
type camuxInfoOut struct {
	SessionID string `json:"session_id"`
	Version   string `json:"version"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
}

func camuxInfo(target string) (*camuxInfoOut, error) {
	out, err := runCamux("info", target, "--json")
	if err != nil {
		return nil, err
	}
	var v camuxInfoOut
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("parse camux info json: %w", err)
	}
	return &v, nil
}

// waitForReady polls camux status until ready or timeout.
func waitForReady(target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		switch camuxStatus(target) {
		case "ready":
			return nil
		case "not-found", "dead":
			return fmt.Errorf("target %s is %s", target, camuxStatus(target))
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("waitForReady: timed out after %s on %s", timeout, target)
}
