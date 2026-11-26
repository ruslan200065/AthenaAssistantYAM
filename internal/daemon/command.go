package daemon

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/kws"
)

var errNoAudio = errors.New("athena: нет аудио со станции")

// wakeSettle covers the audible part of wake.pcm. The file is 1.37 s long,
// but the chime itself ends around 0.4 s and its tail drops below the VAD
// threshold around 0.8 s. Waiting for the whole file made the detector miss
// commands spoken right after the chime; stopping too early made the tail
// count as speech.
const wakeSettle = 800 * time.Millisecond

// captureCommand waits for one spoken command after a wake hit. It listens to
// the unamplified microphone bus, so cloud STT gets clean 16-bit PCM; the
// detector's gain is applied only for VAD decisions. settle is the audible
// part of the wake chime: audio is buffered during it, but its echo must not
// end the command or start the VAD. Returns nil when nobody speaks.
func (d *Daemon) captureCommand(ctx context.Context, max, settle time.Duration) ([]int16, error) {
	d.mu.Lock()
	ok := d.audioOK
	gain := d.settings.Gain
	d.mu.Unlock()
	if !ok {
		return nil, errNoAudio
	}
	if gain <= 0 {
		gain = 1
	}

	ch := d.rawAudio.add(128)
	defer d.rawAudio.remove(ch)

	buf := make([]int16, 0, kws.SampleRate*20)
	start := time.Now()
	settleUntil := start.Add(settle)
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
			return normalizeCommand(trimCommand(buf)), nil
		case pcm := <-ch:
			buf = append(buf, pcm...)
			if time.Now().Before(settleUntil) {
				continue
			}
			rms := rmsOf(pcm) * gain
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
				return normalizeCommand(trimCommand(buf)), nil
			}
		}
	}
}

// normalizeCommand lifts quiet speech to a comfortable level without touching
// already-loud samples.
func normalizeCommand(pcm []int16) []int16 {
	var peak int
	for _, s := range pcm {
		a := int(s)
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	if peak < 100 || peak >= 30000 {
		return pcm
	}
	scale := 30000.0 / float64(peak)
	out := make([]int16, len(pcm))
	for i, s := range pcm {
		v := float64(s) * scale
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
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
