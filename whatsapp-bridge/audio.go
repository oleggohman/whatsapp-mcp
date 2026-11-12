package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
)

// opusHeadMinLen is the minimum size of an OpusHead packet:
// magic(8) + version(1) + channels(1) + preSkip(2) + sampleRate(4) + gain(2) + mapping(1).
const opusHeadMinLen = 19

// opusGranuleRate is the fixed rate Opus granule positions are expressed in (RFC 7845).
const opusGranuleRate = 48000

// analyzeOggOpus extracts the duration from an Ogg Opus file and generates a placeholder waveform
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	var lastGranule uint64
	var preSkip uint16
	var foundOpusHead bool

	for i := 0; i < len(data); {
		// Need a full fixed header (27 bytes) to read page fields
		if i+27 > len(data) {
			break
		}
		if string(data[i:i+4]) != "OggS" {
			i++
			continue
		}

		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		if i+27+numSegments > len(data) {
			break
		}
		pageSize := 27 + numSegments
		for _, segLen := range data[i+27 : i+27+numSegments] {
			pageSize += int(segLen)
		}
		// Truncated page: stop rather than slice out of range
		if i+pageSize > len(data) {
			break
		}

		if !foundOpusHead && pageSeqNum <= 1 {
			pageData := data[i : i+pageSize]
			if headPos := bytes.Index(pageData, []byte("OpusHead")); headPos >= 0 &&
				headPos+opusHeadMinLen <= len(pageData) {
				// PreSkip is a little-endian uint16 at offset 10 from the magic start
				preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
				foundOpusHead = true
			}
		}

		// 0xFFFFFFFFFFFFFFFF means "no granule position" for this page
		if granulePos != 0 && granulePos != math.MaxUint64 {
			lastGranule = granulePos
		}

		i += pageSize
	}

	if lastGranule > uint64(preSkip) {
		durationSeconds := float64(lastGranule-uint64(preSkip)) / opusGranuleRate
		duration = uint32(math.Ceil(durationSeconds))
	} else {
		// No usable granule position: fall back to a rough size-based estimate
		duration = uint32(float64(len(data)) / 2000.0)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	waveform = placeholderWaveform(duration)
	return duration, waveform, nil
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rng := rand.New(rand.NewSource(int64(duration)))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rng.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
