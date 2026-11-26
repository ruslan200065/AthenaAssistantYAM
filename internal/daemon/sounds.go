package daemon

import (
	"context"
	"embed"
	"encoding/binary"
	"log"
	"strings"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/luna"
	"github.com/ruslan200065/AthenaAssistantYAM/internal/wav"
)

// The station's stock signals, copied from the Athena device firmware. They
// are raw 16 kHz mono s16le PCM, so they need no transcoding before Luna.
//
//go:embed sounds/*.pcm
var soundFS embed.FS

const soundRate = 16000

// The stock signals are quite loud on the station speaker. Lower them so they
// stay noticeable without being harsh; TTS volume is left untouched.
var soundGain = map[string]float64{
	"wake":   0.42,
	"listen": 0.50,
	"error":  0.38,
}

func (d *Daemon) playSound(ctx context.Context, lc *luna.Client, name string) time.Duration {
	b, err := soundFS.ReadFile("sounds/" + name + ".pcm")
	if err != nil || len(b) < 2 {
		return 0
	}
	gain := soundGain[name]
	if gain <= 0 {
		gain = 0.5
	}
	pcm := make([]int16, len(b)/2)
	for i := range pcm {
		v := int16(binary.LittleEndian.Uint16(b[2*i:]))
		pcm[i] = int16(float64(v) * gain)
	}
	dur := time.Duration(len(pcm)) * time.Second / soundRate
	if dur > 0 {
		d.muteDetection(dur + 300*time.Millisecond)
	}
	data := wav.Encode(pcm, soundRate)
	var playErr error
	for attempt := 0; attempt < 8; attempt++ {
		playErr = lc.PlayWAV(ctx, data)
		if playErr == nil {
			return dur
		}
		if !strings.Contains(playErr.Error(), "audio output is busy") {
			break
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(250 * time.Millisecond):
		}
	}
	if strings.Contains(playErr.Error(), "audio output is busy") {
		// The previous TTS job can hold the output for a few hundred ms after
		// its audible end. Skipping the cue is safer than reporting an error.
		log.Printf("athena: sound %s skipped: audio output is busy", name)
	} else {
		log.Printf("athena: sound %s: %v", name, playErr)
	}
	return 0
}
