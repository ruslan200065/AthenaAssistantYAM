package daemon

import (
	"context"
	"embed"
	"encoding/binary"
	"log"
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

func (d *Daemon) playSound(ctx context.Context, lc *luna.Client, name string) time.Duration {
	b, err := soundFS.ReadFile("sounds/" + name + ".pcm")
	if err != nil || len(b) < 2 {
		return 0
	}
	pcm := make([]int16, len(b)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	dur := time.Duration(len(pcm)) * time.Second / soundRate
	if dur > 0 {
		d.muteDetection(dur + 300*time.Millisecond)
	}
	if err := lc.PlayWAV(ctx, wav.Encode(pcm, soundRate)); err != nil {
		log.Printf("athena: sound %s: %v", name, err)
		return 0
	}
	return dur
}
