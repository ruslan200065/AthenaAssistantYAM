package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/luna"
)

const (
	ledCount         = 89
	ledErrorColor    = "#FF0000"
	ledErrorDuration = 1500 * time.Millisecond
	ledErrorFrame    = 50 * time.Millisecond
	ledHandoffColor  = "#40C4FF"
	ledHandoffTime   = 1400 * time.Millisecond
	ledHandoffFrame  = 40 * time.Millisecond
	ledHandoffBand   = 9
)

func hsvToRGB(h, s, v float64) (uint8, uint8, uint8) {
	h = math.Mod(h, 1)
	if h < 0 {
		h++
	}
	i := int(h * 6)
	f := h*6 - float64(i)
	p := v * (1 - s)
	q := v * (1 - s*f)
	t := v * (1 - s*(1-f))
	var r, g, b float64
	switch i % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	default:
		r, g, b = v, p, q
	}
	return uint8(r*255 + 0.5), uint8(g*255 + 0.5), uint8(b*255 + 0.5)
}

type ledShow struct {
	cancel      context.CancelFunc
	done        chan struct{}
	stopOnce    sync.Once
	clearOnStop bool
	guardClear  bool
	yielded     atomic.Bool

	frameMu   sync.Mutex
	lastFrame []byte
}

func (s *ledShow) setFrame(frame []byte) {
	s.frameMu.Lock()
	s.lastFrame = append(s.lastFrame[:0], frame...)
	s.frameMu.Unlock()
}

// ownsFramebuffer reports whether the last painted frame is still on screen.
// A passive animation uses this before clearing so it never blanks native
// Alice or another indicator that took over meanwhile.
func (s *ledShow) ownsFramebuffer() bool {
	s.frameMu.Lock()
	last := append([]byte(nil), s.lastFrame...)
	s.frameMu.Unlock()
	if len(last) == 0 {
		return false
	}
	cur, err := readLEDFramebuffer()
	return err == nil && bytes.Equal(cur, last)
}

func (s *ledShow) stop(_ *luna.Client) {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.cancel()
		<-s.done
		if s.clearOnStop && (!s.guardClear || s.ownsFramebuffer()) {
			// Native maind can repaint between clear and Luna's ownership
			// check, so all Station 2 LED writes go straight to the RGBW
			// framebuffer.
			clearLEDFramebuffer()
		}
	})
}

// clearLEDFramebuffer blanks the Station 2 RGBW framebuffer. Luna refuses to
// paint when the framebuffer still belongs to firmware or another indicator;
// a blank frame transfers ownership back to Luna.
func clearLEDFramebuffer() {
	f, err := os.OpenFile("/dev/fb0", os.O_WRONLY, 0)
	if err != nil {
		log.Printf("led: open fb0: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(make([]byte, ledCount*4)); err != nil {
		log.Printf("led: clear fb0: %v", err)
	}
}

// writeLEDFramebuffer paints the Station 2 RGBW matrix directly. Luna refuses
// to write when native firmware owns the framebuffer, and maind can repaint
// between our clear and Luna's ownership check. Direct writes are race-free.
func writeLEDFramebuffer(colors []string) error {
	_, err := writeLEDFramebufferBytes(colors)
	return err
}

func writeLEDFramebufferBytes(colors []string) ([]byte, error) {
	if len(colors) == 0 {
		return nil, nil
	}
	f, err := os.OpenFile("/dev/fb0", os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, ledCount*4)
	for i := 0; i < ledCount; i++ {
		c := colors[0]
		if len(colors) > 1 {
			if i >= len(colors) {
				c = "#000000"
			} else {
				c = colors[i]
			}
		}
		if len(c) != 7 || c[0] != '#' {
			continue
		}
		r, err1 := strconv.ParseUint(c[1:3], 16, 8)
		g, err2 := strconv.ParseUint(c[3:5], 16, 8)
		b, err3 := strconv.ParseUint(c[5:7], 16, 8)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		buf[i*4] = uint8(r)
		buf[i*4+1] = uint8(g)
		buf[i*4+2] = uint8(b)
	}
	if _, err := f.Write(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// stopLEDAnimations invalidates any running animation and fade. Direct writers
// such as the error heartbeat use this so their frame cannot be blended with a
// stale animation frame.
func stopLEDAnimations() {
	ledAnimationOwner.Lock()
	ledAnimationOwner.generation++
	if ledAnimationOwner.cancel != nil {
		ledAnimationOwner.cancel()
		ledAnimationOwner.cancel = nil
	}
	ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
	ledAnimationOwner.Unlock()
}

// pulseErrorLED shows one red heartbeat for 1.5 s and always turns the panel
// off. It is used for explicit failures and cancellations, not for silence
// (nobody spoke) and not for a normal end of the conversation.
func pulseErrorLED(ctx context.Context, lc *luna.Client) {
	stopLEDAnimations()
	clearLEDFramebuffer()
	frames := int(ledErrorDuration / ledErrorFrame)
	for i := 0; i < frames; i++ {
		t := float64(i) / float64(frames)
		// One smooth swell: dim red -> bright red -> dim red. This reads as a
		// single heartbeat rather than three discrete blinks.
		level := 0.55 - 0.30*math.Cos(2*math.Pi*t)
		colors := scaleLEDColors([]string{ledErrorColor}, level)
		if err := writeLEDFramebuffer(colors); err != nil {
			log.Printf("led: error pulse: %v", err)
			break
		}
		if !sleepLED(ctx, ledErrorFrame) {
			break
		}
	}
	clearLEDFramebuffer()
}

// showWakeLostLED is the quiet handoff animation: a cyan comet travels up the
// matrix to show that another station accepted the request. It is deliberately
// not the red error heartbeat and plays no sound.
func (d *Daemon) showWakeLostLED(ctx context.Context) {
	stopLEDAnimations()
	clearLEDFramebuffer()
	frames := int(ledHandoffTime / ledHandoffFrame)
	for f := 0; f < frames; f++ {
		t := float64(f) / float64(frames)
		pos := int(math.Round((1 - t) * float64(ledCount-1)))
		colors := make([]string, ledCount)
		for i := range colors {
			colors[i] = "#000000"
		}
		for k := 0; k < ledHandoffBand; k++ {
			idx := pos + k
			if idx < 0 || idx >= ledCount {
				continue
			}
			level := 1 - float64(k)/float64(ledHandoffBand)
			colors[idx] = scaleLEDColors([]string{ledHandoffColor}, 0.15+0.85*level)[0]
		}
		if err := writeLEDFramebuffer(colors); err != nil {
			log.Printf("led: handoff: %v", err)
			break
		}
		if !sleepLED(ctx, ledHandoffFrame) {
			break
		}
	}
	clearLEDFramebuffer()
}

func sleepLED(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func scaleLEDColors(colors []string, k float64) []string {
	if k >= 1 {
		return colors
	}
	if k < 0 {
		k = 0
	}
	out := make([]string, len(colors))
	for i, c := range colors {
		if len(c) != 7 || c[0] != '#' {
			out[i] = c
			continue
		}
		r, err1 := strconv.ParseUint(c[1:3], 16, 8)
		g, err2 := strconv.ParseUint(c[3:5], 16, 8)
		b, err3 := strconv.ParseUint(c[5:7], 16, 8)
		if err1 != nil || err2 != nil || err3 != nil {
			out[i] = c
			continue
		}
		out[i] = fmt.Sprintf("#%02X%02X%02X",
			uint8(float64(r)*k+0.5), uint8(float64(g)*k+0.5), uint8(float64(b)*k+0.5))
	}
	return out
}

// speechLevel returns a 0..1 envelope sampled from the TTS PCM. The animation
// is started just before playback, so time.Since(start) tracks the voice.
func speechLevel(pcm []int16, rate int) func() float64 {
	const winMS = 20
	if rate <= 0 {
		rate = 16000
	}
	win := rate * winMS / 1000
	if win < 1 {
		win = 1
	}
	levels := make([]float64, 0, len(pcm)/win+1)
	var maxRMS float64
	for off := 0; off < len(pcm); off += win {
		end := min(off+win, len(pcm))
		var sum float64
		for _, s := range pcm[off:end] {
			f := float64(s) / 32768
			sum += f * f
		}
		rms := math.Sqrt(sum / float64(end-off))
		levels = append(levels, rms)
		if rms > maxRMS {
			maxRMS = rms
		}
	}
	start := time.Now()
	return func() float64 {
		if len(levels) == 0 || maxRMS <= 0 {
			return 0.5
		}
		idx := int(time.Since(start).Milliseconds() / winMS)
		if idx < 0 || idx >= len(levels) {
			return 0
		}
		return math.Min(1, levels[idx]/maxRMS)
	}
}
