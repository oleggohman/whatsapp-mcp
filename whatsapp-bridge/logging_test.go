package main

import (
	"strings"
	"testing"
)

func TestStderrLoggerLevelsAndSub(t *testing.T) {
	out := captureStderr(t, func() {
		l := newStderrLogger("Mod", "warn")
		l.Debugf("hidden-debug %d", 1)
		l.Infof("hidden-info %d", 2)
		l.Warnf("shown-warn %d", 3)
		l.Errorf("shown-error %d", 4)
		sub := l.Sub("Child")
		sub.Infof("hidden-sub-info")
		sub.Warnf("shown-sub-warn")
	})
	for _, hidden := range []string{"hidden-debug", "hidden-info", "hidden-sub-info"} {
		if strings.Contains(out, hidden) {
			t.Errorf("%s must be filtered at WARN:\n%s", hidden, out)
		}
	}
	for _, want := range []string{"[Mod WARN] shown-warn 3", "[Mod ERROR] shown-error 4", "[Mod/Child WARN] shown-sub-warn"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStderrLoggerDebugLevel(t *testing.T) {
	out := captureStderr(t, func() {
		newStderrLogger("Mod", "DEBUG").Debugf("dbg %s", "x")
	})
	if !strings.Contains(out, "[Mod DEBUG] dbg x") {
		t.Fatalf("got %q", out)
	}
}
