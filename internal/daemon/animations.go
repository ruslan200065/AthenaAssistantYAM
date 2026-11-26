package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
	"time"
)

type ledMode int

const (
	ledListen ledMode = iota
	ledThink
	ledSpeak
	ledMusic
	ledIdle
)

const (
	ledAnimFrame  = 33 * time.Millisecond
	ledTransition = 420 * time.Millisecond
)

var errLEDForeign = errors.New("led: another writer owns the framebuffer")

// One framebuffer, one local owner. Every write and ownership change is
// serialized. Foreign processes are still detected by framebuffer readback.
var ledAnimationOwner struct {
	sync.Mutex
	generation uint64
	cancel     context.CancelFunc
	colors     []string
	last       []byte
}

func (m ledMode) String() string {
	switch m {
	case ledListen:
		return "listen"
	case ledThink:
		return "think"
	case ledSpeak:
		return "speak"
	case ledMusic:
		return "music"
	default:
		return "idle"
	}
}

func (d *Daemon) startLEDAnimation(ctx context.Context, mode ledMode, level func() float64, requireFree, watch bool) *ledShow {
	ledCtx, cancel := context.WithCancel(ctx)
	show := &ledShow{
		cancel: cancel,
		done:   make(chan struct{}),
		// This file owns cleanup. A legacy stop() must not clear after a handoff.
		clearOnStop: false,
		guardClear:  watch,
	}
	ledAnimationOwner.Lock()
	if ledAnimationOwner.cancel != nil {
		ledAnimationOwner.cancel()
	}
	ledAnimationOwner.generation++
	generation := ledAnimationOwner.generation
	ledAnimationOwner.cancel = cancel
	ledAnimationOwner.Unlock()
	log.Printf("led: start %s", mode)
	go func() {
		defer close(show.done)
		defer cancel()
		d.playLEDAnimation(ledCtx, show, mode, level, requireFree, watch, generation)
	}()
	return show
}

func (d *Daemon) playLEDAnimation(ctx context.Context, show *ledShow, mode ledMode, level func() float64, requireFree, watch bool, generation uint64) {
	w := &ledFrameWriter{}
	from := ledBlackFrame()
	ledAnimationOwner.Lock()
	if generation != ledAnimationOwner.generation {
		ledAnimationOwner.Unlock()
		return
	}
	if ledAnimationOwner.last != nil {
		cur, err := readLEDFramebuffer()
		if err == nil && bytes.Equal(cur, ledAnimationOwner.last) {
			from = append([]string(nil), ledAnimationOwner.colors...)
			w.last = append([]byte(nil), ledAnimationOwner.last...)
		} else {
			ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
		}
	}
	ledAnimationOwner.Unlock()

	start := time.Now()
	previous := start
	smoothed := 0.0
	ticker := time.NewTicker(ledAnimFrame)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			// Finite cleanup does not hold up show.done, so stop-then-start callers
			// can crossfade from the previous frame rather than from black.
			go ledFadeOut(generation)
			return
		}
		now := time.Now()
		dt := now.Sub(previous).Seconds()
		previous = now
		if dt <= 0 {
			dt = ledAnimFrame.Seconds()
		}
		target := 0.0
		if level != nil {
			target = clamp01(level())
		}
		tau := 0.18 // release
		if target > smoothed {
			tau = 0.055
		} // attack
		smoothed += (target - smoothed) * (1 - math.Exp(-dt/tau))
		elapsed := now.Sub(start)
		colors := animationFrame(mode, elapsed.Seconds(), smoothed)
		if elapsed < ledTransition {
			colors = ledBlendFrames(from, colors, ledSmoothstep(float64(elapsed)/float64(ledTransition)))
		}
		ledAnimationOwner.Lock()
		if generation != ledAnimationOwner.generation {
			ledAnimationOwner.Unlock()
			return
		}
		if ctx.Err() != nil {
			ledAnimationOwner.Unlock()
			go ledFadeOut(generation)
			return
		}
		err := w.write(colors, requireFree, watch)
		if err == nil {
			ledAnimationOwner.colors = colors
			ledAnimationOwner.last = append([]byte(nil), w.last...)
		} else if errors.Is(err, errLEDForeign) {
			ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
		}
		ledAnimationOwner.Unlock()
		if err != nil {
			if errors.Is(err, errLEDForeign) {
				log.Printf("led: %s yielded to another writer", mode)
				show.yielded.Store(true)
			} else {
				log.Printf("led: %s: %v", mode, err)
			}
			return
		}
		show.setFrame(w.last)
		select {
		case <-ctx.Done():
			go ledFadeOut(generation)
			return
		case <-ticker.C:
		}
	}
}

// Caller holds ledAnimationOwner.Lock(). Never clear a foreign-owned frame.
func ledClearOwnedLocked(allowUnowned bool) error {
	if ledAnimationOwner.last != nil {
		cur, err := readLEDFramebuffer()
		if err != nil {
			return err
		}
		if !bytes.Equal(cur, ledAnimationOwner.last) {
			ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
			return errLEDForeign
		}
	} else if !allowUnowned {
		return nil
	}
	black := ledBlackFrame()
	_, err := writeLEDFramebufferBytes(black)
	if err == nil {
		ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
	}
	return err
}

// A cancellation without a new mode fades to black. A new generation takes
// over immediately and cancels this fade by invalidating its generation.
func ledFadeOut(generation uint64) {
	start := time.Now()
	ledAnimationOwner.Lock()
	if generation != ledAnimationOwner.generation || ledAnimationOwner.last == nil {
		ledAnimationOwner.Unlock()
		return
	}
	from := append([]string(nil), ledAnimationOwner.colors...)
	w := &ledFrameWriter{last: append([]byte(nil), ledAnimationOwner.last...)}
	ledAnimationOwner.Unlock()
	black := ledBlackFrame()
	ticker := time.NewTicker(ledAnimFrame)
	defer ticker.Stop()
	for {
		<-ticker.C
		p := clamp01(float64(time.Since(start)) / float64(ledTransition))
		colors := ledBlendFrames(from, black, ledSmoothstep(p))
		ledAnimationOwner.Lock()
		if generation != ledAnimationOwner.generation {
			ledAnimationOwner.Unlock()
			return
		}
		err := w.write(colors, false, true)
		if err != nil {
			if errors.Is(err, errLEDForeign) {
				ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
			} else {
				log.Printf("led: fade: %v", err)
			}
			ledAnimationOwner.Unlock()
			return
		}
		if p >= 1 {
			ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
			ledAnimationOwner.cancel = nil
			ledAnimationOwner.Unlock()
			return
		}
		ledAnimationOwner.colors = colors
		ledAnimationOwner.last = append([]byte(nil), w.last...)
		ledAnimationOwner.Unlock()
	}
}

// animationFrame returns one frame for the Station 2 ring. The station has 89
// RGBW LEDs arranged in a circle; the source animation was written for a 12x7
// matrix, so the matrix effects are translated to circular waves, comets and
// ribbons here. Music stays one-dimensional and uses all 89 LEDs.
func animationFrame(mode ledMode, t, level float64) []string {
	out := ledBlackFrame()
	level = clamp01(level)
	for i := range out {
		a := 2 * math.Pi * float64(i) / float64(ledCount)
		x := float64(i) / float64(ledCount)
		var h, s, v float64
		switch mode {
		case ledListen: // two sonar pulses travelling around the ring
			p := frac(t * .65)
			p1 := p
			p2 := frac(p + .55)
			ring1 := ledBell(ringDist(x, p1), .030+.014*level)
			ring2 := .52 * ledBell(ringDist(x, p2), .026+.011*level)
			breathe := .5 + .5*math.Sin(4*a-2.2*t)
			// Compensate for the startup crossfade so the white flash is visible.
			flash := ledBell(t-.25, .29)
			h = (176 + 24*clamp01(ring1+ring2)) / 360
			s = .90 - .52*flash
			v = .028 + .05*level + .09*breathe*(.35+.65*level) + (.56+.38*level)*ring1 + (.30+.24*level)*ring2 + .65*flash
		case ledThink: // two orbiting comets with fading tails
			q := t * 1.85
			p1, p2 := 0.0, 0.0
			for k := 0; k < 9; k++ {
				u := q - float64(k)*.17
				w := math.Pow(1-float64(k)/10, 2)
				pos1 := frac(u / (2 * math.Pi))
				pos2 := frac(u/(2*math.Pi) + .5)
				p1 = math.Max(p1, w*ledBell(ringDist(x, pos1), .026))
				p2 = math.Max(p2, w*ledBell(ringDist(x, pos2), .026))
			}
			twinkle := math.Pow(math.Max(0, math.Sin(2*math.Pi*(x*7.3+t*.9))), 18) * .30
			h = ledMix(186, 269, p2/(p1+p2+.001)) / 360
			s = .95 - .50*math.Max(p1, p2)
			v = .018 + .055*(.5+.5*math.Sin(2*a-t*.8)) + .98*p1 + .84*p2 + twinkle
		case ledSpeak: // two audio-reactive aurora ribbons
			p1 := frac(t * .20)
			p2 := frac(.55 - t*.13)
			b1 := ledBell(ringDist(x, p1), .090+.065*level)
			b2 := ledBell(ringDist(x, p2), .105+.050*level)
			w1 := .5 + .5*math.Sin(4*a+2.7*t)
			w2 := .5 + .5*math.Sin(3*a-1.9*t+1.7)
			shimmer := math.Pow(math.Max(0, math.Sin(2*math.Pi*(x*6.4+t*1.8))), 14) * .10
			h = ledMix(163, 284, b2/(b1+b2+.001)) / 360
			s = .92 - .46*clamp01(b1+b2)
			v = .022 + (.15+.42*level)*(b1+b2) + (.05+.14*level)*(w1+w2)*.5 + shimmer*(.35+.65*level)
		case ledMusic: // rotating rainbow with a synthetic beat and sparkles
			beat := math.Exp(-7 * frac(t*1.9))
			w1 := .5 + .5*math.Sin(6*a-5*t)
			w2 := .5 + .5*math.Sin(3*a+2.2*t)
			spark := math.Pow(math.Max(0, math.Sin(2*math.Pi*(x*5.7+t*1.25))), 22)
			h = frac(x + .08*t)
			s = .92
			v = clamp01(.10 + .20*w1 + .26*beat + .18*w1*w2 + .45*spark*(.35+beat))
		default: // ledIdle: a calm, very dim breathing aurora
			w1 := .5 + .5*math.Sin(2*a+.55*t)
			w2 := .5 + .5*math.Sin(3*a-.35*t+1.8)
			h = .55 + .025*math.Sin(.20*t)
			s = .78
			v = .042 + .065*w1 + .035*w2
		}
		out[i] = hsvString(h, clamp01(s), clamp01(v))
	}
	return out
}

func ledBlackFrame() []string {
	out := make([]string, ledCount)
	for i := range out {
		out[i] = "#000000"
	}
	return out
}

func ledBlendFrames(a, b []string, p float64) []string {
	p = clamp01(p)
	if p >= 1 {
		return b
	}
	out := make([]string, len(b))
	for i := range b {
		var x uint64
		if i < len(a) && len(a[i]) == 7 {
			x, _ = strconv.ParseUint(a[i][1:], 16, 24)
		}
		var y uint64
		if len(b[i]) == 7 {
			y, _ = strconv.ParseUint(b[i][1:], 16, 24)
		}
		r := uint8(math.Round(ledMix(float64((x>>16)&255), float64((y>>16)&255), p)))
		g := uint8(math.Round(ledMix(float64((x>>8)&255), float64((y>>8)&255), p)))
		bl := uint8(math.Round(ledMix(float64(x&255), float64(y&255), p)))
		out[i] = fmt.Sprintf("#%02X%02X%02X", r, g, bl)
	}
	return out
}

func ledBell(x, s float64) float64    { return math.Exp(-math.Pow(x/s, 2)) }
func ledMix(a, b, p float64) float64  { return a + (b-a)*p }
func ledSmoothstep(x float64) float64 { x = clamp01(x); return x * x * (3 - 2*x) }
func ringDist(a, b float64) float64   { d := math.Abs(a - b); return math.Min(d, 1-d) }
func frac(v float64) float64          { return v - math.Floor(v) }
func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
func hsvString(h, s, v float64) string {
	r, g, b := hsvToRGB(h, s, v)
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

type ledFrameWriter struct{ last []byte }

func (w *ledFrameWriter) write(colors []string, requireFree, watch bool) error {
	if requireFree || watch {
		cur, err := readLEDFramebuffer()
		// Fail closed: without readback, ownership cannot be checked safely.
		if err != nil {
			return err
		}
		if w.last == nil {
			if requireFree && !dimFrame(cur) {
				return errLEDForeign
			}
		} else if watch && !bytes.Equal(cur, w.last) {
			return errLEDForeign
		}
	}
	buf, err := writeLEDFramebufferBytes(colors)
	if err != nil {
		return err
	}
	w.last = buf
	return nil
}

func readLEDFramebuffer() ([]byte, error) {
	f, err := os.Open("/dev/fb0")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, ledCount*4)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func dimFrame(buf []byte) bool {
	for i := 0; i+2 < len(buf); i += 4 {
		if max(buf[i], buf[i+1], buf[i+2]) > 64 {
			return false
		}
	}
	return true
}
