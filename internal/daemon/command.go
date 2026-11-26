package daemon

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/kws"
)

var errNoAudio = errors.New("athena: нет аудио со станции")

// captureCommand waits for one spoken command after a wake hit. It uses the
// same processed microphone stream as the detector, so no extra ALSA or IPC
// subscription is opened. Returns nil when nobody speaks.
func (d *Daemon) captureCommand(ctx context.Context, max time.Duration) ([]int16, error) {
	d.mu.Lock()
	ok := d.audioOK
	pre := d.ring.last(kws.SampleRate * 150 / 1000) // 150 ms pre-roll
	d.mu.Unlock()
	if !ok {
		return nil, errNoAudio
	}

	ch := d.audio.add(128)
	defer d.audio.remove(ch)

	buf := append([]int16(nil), pre...)
	start := time.Now()
	waitSpeech := 4 * time.Second
	if max < waitSpeech {
		waitSpeech = max
	}
	timer := time.NewTimer(max)
	defer timer.Stop()

	noise := 0.003
	const floorRMS = 0.012
	speaking := false
	var silenceMS, voicedMS int

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			if !speaking || voicedMS < 120 {
				return nil, nil
			}
			return trimCommand(buf), nil
		case pcm := <-ch:
			buf = append(buf, pcm...)
			rms := rmsOf(pcm)
			ms := len(pcm) * 1000 / kws.SampleRate
			if ms <= 0 {
				continue
			}
			thr := math.Max(floorRMS, noise*2.8)
			if !speaking {
				if rms < noise*3 {
					noise = 0.98*noise + 0.02*rms
				}
				if rms >= thr {
					speaking = true
					voicedMS, silenceMS = ms, 0
				} else if time.Since(start) >= waitSpeech {
					return nil, nil
				}
				continue
			}
			if rms >= thr {
				voicedMS += ms
				silenceMS = 0
			} else {
				silenceMS += ms
			}
			if silenceMS >= 800 {
				if voicedMS < 120 {
					return nil, nil
				}
				return trimCommand(buf), nil
			}
		}
	}
}

// trimCommand drops leading/trailing room noise while keeping a little
// context for the recognizer.
func trimCommand(pcm []int16) []int16 {
	s, e, ok := kws.FindSpeech(pcm)
	if !ok {
		return pcm
	}
	pad := kws.SampleRate / 10 // 100 ms
	s = max(0, s-pad)
	e = min(len(pcm), e+pad)
	return pcm[s:e]
}

func rmsOf(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		f := float64(s) / 32768
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(pcm)))
}
