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

// settleFor covers the follow-up chime plus a little tail.
func settleFor(sound time.Duration) time.Duration {
	s := sound + 100*time.Millisecond
	if s < 300*time.Millisecond {
		s = 300 * time.Millisecond
	}
	return s
}

// captureCommand waits for one spoken command after a wake hit. It listens to
// the unamplified microphone bus, so cloud STT gets clean 16-bit PCM; the
// detector's gain is applied only for VAD decisions. settle covers the wake
// chime: audio during it is ignored so the chime is not mistaken for speech
// and is not sent to the recognizer. Returns nil when nobody speaks.
func (d *Daemon) captureCommand(ctx context.Context, max, settle, waitFor time.Duration, onLevel func(float64)) ([]int16, error) {
	d.mu.Lock()
	ok := d.audioOK
	d.mu.Unlock()
	if !ok {
		return nil, errNoAudio
	}

	ch := d.rawAudio.add(128)
	defer d.rawAudio.remove(ch)
	if onLevel != nil {
		onLevel(0)
		defer onLevel(0)
	}

	buf := make([]int16, 0, kws.SampleRate*20)
	start := time.Now()
	settleUntil := start.Add(settle)
	// wait_for_speech_seconds is the window in which the user may start
	// speaking. It starts after the wake chime has died down, not while the
	// station is still playing its own sound, otherwise a third of the core's
	// window is lost before the microphone is effectively open.
	waitSpeech := settle + waitFor
	if waitFor <= 0 {
		waitSpeech = settle + 4*time.Second
	}
	if max < waitSpeech {
		waitSpeech = max
	}
	timer := time.NewTimer(max)
	defer timer.Stop()

	// This VAD runs on the unamplified microphone bus. The detector gain
	// (default 32) is tuned for the wake-word frontend; applying it here made
	// ordinary room noise look like speech at -39 dB, so the command capture
	// started on noise and sent a random 0.8–1.5 s fragment to STT.
	noise := 0.002
	const floorRMS = 0.006
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
			if time.Now().Before(settleUntil) {
				continue
			}
			buf = append(buf, pcm...)
			rms := rmsOf(pcm)
			if onLevel != nil {
				onLevel(math.Min(1, rms*8))
			}
			ms := len(pcm) * 1000 / kws.SampleRate
			if ms <= 0 {
				continue
			}
			thr := math.Max(floorRMS, noise*3.0)
			if !speaking {
				if rms < noise*2.0 {
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
			if silenceMS >= 1200 {
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
