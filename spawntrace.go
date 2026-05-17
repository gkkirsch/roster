package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Opt-in structured tracer for the ensure/spawn path. Mirror of
// camux/trace.go: activated by ROSTER_SPAWN_TRACE=<path>. Same
// tab-separated key=value format.
//
// roster + camux can share a single file: each process writes a
// `=== roster <pid> <ts> argv=… ===` (or `=== camux …`) banner before
// its events, so the combined file reads chronologically across one
// ensure → camux-spawn cycle.

var (
	spawnTraceFile     *os.File
	spawnTraceMu       sync.Mutex
	spawnTraceInitOnce sync.Once
)

func spawnTraceInit() {
	spawnTraceInitOnce.Do(func() {
		path := os.Getenv("ROSTER_SPAWN_TRACE")
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roster: spawn-trace open %s: %v\n", path, err)
			return
		}
		spawnTraceFile = f
		_, _ = fmt.Fprintf(f, "\n=== roster %d %s argv=%v ===\n",
			os.Getpid(), time.Now().Format(time.RFC3339Nano), os.Args)
	})
}

func spawnTrace(category string, kv ...string) {
	spawnTraceInit()
	if spawnTraceFile == nil {
		return
	}
	var sb strings.Builder
	sb.WriteString(time.Now().Format("15:04:05.000"))
	sb.WriteByte('\t')
	sb.WriteString(category)
	for i := 0; i+1 < len(kv); i += 2 {
		sb.WriteByte('\t')
		sb.WriteString(kv[i])
		sb.WriteByte('=')
		sb.WriteString(spawnTraceQuote(kv[i+1]))
	}
	sb.WriteByte('\n')
	spawnTraceMu.Lock()
	_, _ = spawnTraceFile.WriteString(sb.String())
	spawnTraceMu.Unlock()
}

func spawnTraceQuote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"\\") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func spawnTraceErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
