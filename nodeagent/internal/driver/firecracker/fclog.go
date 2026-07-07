//go:build firecracker && linux

package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tavon-ai/proteos/nodeagent/internal/metrics"
)

// logFIFOName is the filename of the Firecracker log FIFO inside the jail run dir.
const logFIFOName = "fc.log"

// logFIFOJailPath is the log path as Firecracker sees it inside its chroot.
const logFIFOJailPath = "/run/" + logFIFOName

// logFIFOPath returns the host-side absolute path of the Firecracker log FIFO.
func (l jailLayout) logFIFOPath() string {
	return filepath.Join(l.root(), "run", logFIFOName)
}

// createLogFIFO makes the named pipe for Firecracker's structured log output
// and chowns it to the jail uid so the VMM process can open it for writing.
func createLogFIFO(l jailLayout, uid int) error {
	p := l.logFIFOPath()
	_ = os.Remove(p) // clear any stale entry from a previous boot
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		return fmt.Errorf("mkfifo %s: %w", p, err)
	}
	return os.Chown(p, uid, uid)
}

// startLogReader opens the FIFO for reading and tails JSON log lines emitted
// by the Firecracker VMM identified by machineID, forwarding each to slog at
// the appropriate level and incrementing the FCLogLinesTotal counter.
//
// The goroutine exits when ctx is cancelled (which closes the underlying file)
// or when the FIFO write side is permanently closed (VMM exited).
func startLogReader(ctx context.Context, l jailLayout, machineID string) {
	go func() {
		// Open O_RDWR so the call returns immediately rather than blocking until
		// Firecracker opens the write end. We hold both ends of the FIFO; bytes
		// written by Firecracker appear on the read side as usual. Closing the
		// file when ctx is done unblocks the scanner loop.
		f, err := os.OpenFile(l.logFIFOPath(), os.O_RDWR, os.ModeNamedPipe)
		if err != nil {
			slog.Warn("fc log: open fifo", "machine", machineID, "err", err)
			return
		}
		defer f.Close()

		go func() {
			<-ctx.Done()
			_ = f.Close()
		}()

		sc := bufio.NewScanner(f)
		for sc.Scan() {
			emitFCLogLine(machineID, sc.Text())
		}
	}()
}

type fcLogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// emitFCLogLine parses one JSON log line from Firecracker, routes it through
// slog at the matching level, and increments the FCLogLinesTotal counter.
func emitFCLogLine(machineID, line string) {
	var entry fcLogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		slog.Debug("fc log: unparsed line", "machine", machineID, "line", line)
		return
	}
	level := entry.Level
	if level == "" {
		level = "UNKNOWN"
	}
	metrics.FCLogLinesTotal.WithLabelValues(level).Inc()
	switch level {
	case "Error", "ERROR":
		slog.Error("fc", "machine", machineID, "msg", entry.Message)
	case "Warn", "WARN", "Warning", "WARNING":
		slog.Warn("fc", "machine", machineID, "msg", entry.Message)
	case "Debug", "DEBUG":
		slog.Debug("fc", "machine", machineID, "msg", entry.Message)
	default:
		slog.Info("fc", "machine", machineID, "msg", entry.Message)
	}
}
