package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
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

// preflightNotify checks whether the target's TUI can accept input
// right now. claude-code's TUI buffers and queues whatever you type
// while the agent is streaming, so we deliberately DO NOT block on
// "streaming" — we paste, claude queues, the input lands on the next
// turn. That mirrors how a human user types into the chat box mid-run.
//
// We only hard-fail on states where input would be silently lost or
// consumed by something other than the conversation:
//
//   dead / not-found     — there's no TUI to type into
//   stopped              — caller needs to `roster resume` first
//   permission-dialog    — typed text would answer the dialog
//   trust-dialog         — same; would answer the trust prompt
//
// Anything else (ready, streaming, or any unfamiliar state we don't
// have an opinion on) is allowed through. Better to attempt and let
// claude buffer than to block on a state-string we haven't enumerated.
//
// The previous waitForReady gate polled until "ready" with a 30s
// timeout. That gate was the source of duplicate-message bugs:
// concurrent `roster notify` invocations would each independently
// wait, then all fire once the worker finally became ready. Removing
// the ready-gate (and instead leaning on claude's own input queue)
// means there's nothing to "wait for" in parallel.
func preflightNotify(target string) error {
	st := camuxStatus(target)
	switch st {
	case "dead", "not-found":
		return fmt.Errorf("target %s is %s", target, st)
	case "stopped":
		return fmt.Errorf("target %s is stopped — run `roster resume` first", target)
	case "permission-dialog", "trust-dialog":
		return fmt.Errorf("target %s is on a %s — clear it (camux interrupt) before sending", target, st)
	}
	return nil
}
