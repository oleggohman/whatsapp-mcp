package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// The bridge may run as a headless daemon, so stdout stays unused: every log line goes to stderr.

// logf writes a formatted line to stderr.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

// logln writes a line to stderr.
func logln(args ...any) {
	fmt.Fprintln(os.Stderr, args...)
}

// stderrLogger is a waLog.Logger that writes to stderr.
type stderrLogger struct {
	mod string
	min int
}

var logLevels = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

// newStderrLogger returns a whatsmeow logger writing to stderr at minLevel or above.
func newStderrLogger(module, minLevel string) waLog.Logger {
	return &stderrLogger{mod: module, min: logLevels[strings.ToUpper(minLevel)]}
}

func (s *stderrLogger) outputf(level, msg string, args ...any) {
	if logLevels[level] < s.min {
		return
	}
	fmt.Fprintf(os.Stderr, "%s [%s %s] %s\n", time.Now().Format("15:04:05.000"), s.mod, level, fmt.Sprintf(msg, args...))
}

func (s *stderrLogger) Errorf(msg string, args ...any) { s.outputf("ERROR", msg, args...) }
func (s *stderrLogger) Warnf(msg string, args ...any)  { s.outputf("WARN", msg, args...) }
func (s *stderrLogger) Infof(msg string, args ...any)  { s.outputf("INFO", msg, args...) }
func (s *stderrLogger) Debugf(msg string, args ...any) { s.outputf("DEBUG", msg, args...) }
func (s *stderrLogger) Sub(mod string) waLog.Logger {
	return &stderrLogger{mod: s.mod + "/" + mod, min: s.min}
}
