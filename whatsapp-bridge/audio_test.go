package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
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

func TestAnalyzeOggOpusFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/sample-3s.ogg")
	if err != nil {
		t.Fatal(err)
	}
	d, w, err := analyzeOggOpus(data)
	if err != nil {
		t.Fatal(err)
	}
	if d < 3 || d > 4 {
		t.Fatalf("duration %d not in [3,4]", d)
	}
	if len(w) != 64 {
		t.Fatalf("waveform len %d, want 64", len(w))
	}
}

func TestAnalyzeOggOpusNotOgg(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("Og"), []byte("RIFF....WAVE")} {
		if _, _, err := analyzeOggOpus(in); err == nil {
			t.Fatalf("input %q: expected error", in)
		}
	}
}

func TestAnalyzeOggOpusNoPanic(t *testing.T) {
	data, err := os.ReadFile("testdata/sample-3s.ogg")
	if err != nil {
		t.Fatal(err)
	}
	// every truncated prefix must not panic
	for n := 0; n <= len(data); n++ {
		n := n
		t.Run(fmt.Sprintf("prefix-%d", n), func(t *testing.T) {
			_, _, _ = analyzeOggOpus(data[:n])
		})
	}
	// random bytes behind a valid signature, and a corrupted fixture
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		buf := make([]byte, rng.Intn(300))
		rng.Read(buf)
		if len(buf) >= 4 {
			copy(buf, "OggS")
		}
		_, _, _ = analyzeOggOpus(buf)

		c := append([]byte(nil), data...)
		for j := 0; j < 5; j++ {
			c[rng.Intn(len(c))] = byte(rng.Intn(256))
		}
		_, _, _ = analyzeOggOpus(c)
	}
}

func TestAnalyzeOggOpusPreSkipUnderflow(t *testing.T) {
	// one page, granule 10 < preSkip 312: must not underflow, falls back to min duration 1
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1
	head[9] = 1
	binary.LittleEndian.PutUint16(head[10:], 312)
	page := make([]byte, 27, 27+1+len(head))
	copy(page, "OggS")
	binary.LittleEndian.PutUint64(page[6:], 10)
	page[26] = 1
	page = append(page, byte(len(head)))
	page = append(page, head...)
	d, _, err := analyzeOggOpus(page)
	if err != nil || d != 1 {
		t.Fatalf("got d=%d err=%v, want 1,nil", d, err)
	}
}
