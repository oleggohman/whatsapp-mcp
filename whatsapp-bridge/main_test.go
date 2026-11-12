package main

import (
	"bytes"
	"testing"
)

func TestPlaceholderWaveform(t *testing.T) {
	for _, d := range []uint32{0, 1, 5, 30, 120, 600} {
		w := placeholderWaveform(d)
		if len(w) != 64 {
			t.Fatalf("duration %d: want len 64, got %d", d, len(w))
		}
		for i, v := range w {
			if v > 100 {
				t.Fatalf("duration %d: value %d at %d out of range 0-100", d, v, i)
			}
		}
		if !bytes.Equal(w, placeholderWaveform(d)) {
			t.Fatalf("duration %d: waveform not deterministic", d)
		}
	}
	if bytes.Equal(placeholderWaveform(5), placeholderWaveform(50)) {
		t.Fatal("different durations should give different waveforms")
	}
}
