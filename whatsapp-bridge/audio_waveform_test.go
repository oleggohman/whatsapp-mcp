package main

import "testing"

// The synthetic waveform is clamped to WhatsApp's 0..100 range; some durations saturate it.
func TestPlaceholderWaveformClampsToRange(t *testing.T) {
	saturated := false
	for d := uint32(1); d <= 300; d++ {
		w := placeholderWaveform(d)
		if len(w) != 64 {
			t.Fatalf("duration %d: len %d", d, len(w))
		}
		for _, v := range w {
			if v > 100 {
				t.Fatalf("duration %d: sample %d above 100", d, v)
			}
			if v == 100 {
				saturated = true
			}
		}
	}
	if !saturated {
		t.Fatal("no duration reached the upper clamp")
	}
}
