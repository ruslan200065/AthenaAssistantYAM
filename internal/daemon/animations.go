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
	ledAnimFrame       = 20 * time.Millisecond
	ledTransition      = 420 * time.Millisecond
	ledPanelWidth      = 15 // coordinate grid, NOT a rectangular framebuffer
	ledPanelHeight     = 13
	ledPanelCount      = 84
	ledOknixCount      = 5
	ledPixelCount      = ledPanelCount + ledOknixCount
	ledPanelFlipX      = false
	ledPanelFlipY      = false
	ledOknixReverse    = false
	ledFramebufferPath = "/dev/fb0"
)

type ledPoint struct{ x, y int }

// Coordinates from the union of blink.higher_points, middle_points and
// lower_points in the native led-config.json. Ordered by y, then x.
// Native thinking/volume frames form the expected 14-pixel central contour
// with this order; no serpentine remapping is applied.
var ledPanelPoints = [ledPanelCount]ledPoint{
	{1, 0}, {3, 0}, {5, 0}, {7, 0}, {9, 0}, {11, 0}, {13, 0},
	{0, 1}, {2, 1}, {4, 1}, {6, 1}, {8, 1}, {10, 1}, {12, 1}, {14, 1},
	{1, 2}, {3, 2}, {5, 2}, {7, 2}, {9, 2}, {11, 2}, {13, 2},
	{0, 3}, {2, 3}, {4, 3}, {6, 3}, {8, 3}, {10, 3}, {12, 3}, {14, 3},
	{1, 4}, {3, 4}, {5, 4}, {7, 4}, {9, 4}, {11, 4}, {13, 4},
	{0, 5}, {2, 5}, {12, 5}, {14, 5},
	{1, 6}, {13, 6},
	{0, 7}, {2, 7}, {12, 7}, {14, 7},
	{1, 8}, {3, 8}, {5, 8}, {7, 8}, {9, 8}, {11, 8}, {13, 8},
	{0, 9}, {2, 9}, {4, 9}, {6, 9}, {8, 9}, {10, 9}, {12, 9}, {14, 9},
	{1, 10}, {3, 10}, {5, 10}, {7, 10}, {9, 10}, {11, 10}, {13, 10},
	{0, 11}, {2, 11}, {4, 11}, {6, 11}, {8, 11}, {10, 11}, {12, 11}, {14, 11},
	{1, 12}, {3, 12}, {5, 12}, {7, 12}, {9, 12}, {11, 12}, {13, 12},
}

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
	if mode == ledIdle {
		// Idle has no animation loop. Only a finite transition from our own frame.
		if ledAnimationOwner.last != nil {
			ledAnimationOwner.Unlock()
			go func() {
				defer close(show.done)
				defer cancel()
				if err := ledFadeOut(generation); err != nil {
					if errors.Is(err, errLEDForeign) {
						show.yielded.Store(true)
					} else {
						log.Printf("led: idle: %v", err)
					}
				}
			}()
			return show
		}
		// No owned frame: do not erase a third-party effect in Idle.
		ledAnimationOwner.cancel = nil
		ledAnimationOwner.Unlock()
		cancel()
		close(show.done)
		return show
	}
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
		if (err == nil && bytes.Equal(cur, ledAnimationOwner.last)) || (err != nil && !watch && !requireFree) {
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

// A cancellation without a new mode fades to black. A new generation takes
// over immediately and cancels this fade by invalidating its generation.
func ledFadeOut(generation uint64) error {
	start := time.Now()
	ledAnimationOwner.Lock()
	if generation != ledAnimationOwner.generation || ledAnimationOwner.last == nil {
		ledAnimationOwner.Unlock()
		return nil
	}
	from := append([]string(nil), ledAnimationOwner.colors...)
	w := &ledFrameWriter{last: append([]byte(nil), ledAnimationOwner.last...), strictRead: true}
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
			return nil
		}
		err := w.write(colors, false, true)
		if err != nil {
			if errors.Is(err, errLEDForeign) {
				ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
			} else {
				log.Printf("led: fade: %v", err)
			}
			ledAnimationOwner.Unlock()
			return err
		}
		if p >= 1 {
			ledAnimationOwner.colors, ledAnimationOwner.last = nil, nil
			ledAnimationOwner.cancel = nil
			ledAnimationOwner.Unlock()
			return nil
		}
		ledAnimationOwner.colors = colors
		ledAnimationOwner.last = append([]byte(nil), w.last...)
		ledAnimationOwner.Unlock()
	}
}

// animationFrame renders exactly one frame in native framebuffer index order.
func animationFrame(mode ledMode, t, level float64) []string {
	out := ledBlackFrame()
	if mode == ledIdle || len(out) != ledPixelCount {
		return out
	}
	level = clamp01(level)
	if mode == ledMusic {
		// Preserve the original music effect, including the five extra pixels.
		for i := range out {
			a := 2 * math.Pi * float64(i) / float64(ledCount)
			x := float64(i) / float64(ledCount)
			beat := math.Exp(-7 * frac(t*1.9))
			w1 := .5 + .5*math.Sin(6*a-5*t)
			w2 := .5 + .5*math.Sin(3*a+2.2*t)
			spark := math.Pow(math.Max(0, math.Sin(2*math.Pi*(x*5.7+t*1.25))), 22)
			out[i] = hsvString(frac(x+.08*t), .92, clamp01(.10+.20*w1+.26*beat+.18*w1*w2+.45*spark*(.35+beat)))
		}
		return out
	}
	for i, p := range ledPanelPoints {
		x, y := (float64(p.x)-7)/4.5, (float64(p.y)-6)/4.5
		if ledPanelFlipX {
			x = -x
		}
		if ledPanelFlipY {
			y = -y
		}
		h, sat, v := ledPanelPixel(mode, x, y, t, level)
		out[i] = ledVoiceColor(h, sat, v)
	}
	for j := 0; j < ledOknixCount; j++ {
		h, sat, v := ledOknixPixel(mode, j, t, level)
		k := j
		if ledOknixReverse {
			k = ledOknixCount - 1 - j
		}
		out[ledPanelCount+k] = ledVoiceColor(h, sat, v)
	}
	return out
}

// Pulse envelope smoothly hides the radius reset between waves.
func ledSonarPulse(r, t, offset, width float64) float64 {
	p := frac(t*.65 + offset)
	env := ledSmoothstep(p/.07) * ledSmoothstep((1-p)/.12)
	return env * ledBell(r-(.45+p*1.85), width)
}

func ledPanelPixel(mode ledMode, x, y, t, l float64) (h, s, v float64) {
	r := math.Hypot(x, y)
	// The central opening contains no LEDs. Its visible contour is roughly
	// elliptical; use it as a luminous source instead of a fictitious center.
	inner := math.Hypot(x/1.12, y/.52)
	switch mode {
	case ledListen:
		ring := ledSonarPulse(r, t, 0, .22+.14*l)
		ring2 := .45 * ledSonarPulse(r, t, .55, .20)
		core := ledBell(inner-1, .36+.12*l)
		return 184 + 22*clamp01(r/2), .9, .008 + (.48+.5*l)*ring + ring2 + core*(.18+.42*l)
	case ledThink:
		// Two comets orbit the actual central opening. Width is chosen for
		// the native staggered pixel pitch, so heads never vanish in gaps.
		p1, p2 := 0.0, 0.0
		q := t * 1.85
		for k := 0; k < 11; k++ {
			u := q - float64(k)*.16
			w := math.Pow(1-float64(k)/12, 2)
			cx, cy := 1.13*math.Cos(u), .54*math.Sin(u)
			p1 = math.Max(p1, w*ledBell(math.Hypot(x-cx, y-cy), .32))
			p2 = math.Max(p2, w*ledBell(math.Hypot(x+cx, y+cy), .32))
		}
		path := ledBell(inner-1, .25)
		return ledMix(186, 269, p2/(p1+p2+.001)), .95 - .56*math.Max(p1, p2), .006 + .075*path + .98*p1 + .86*p2
	case ledSpeak:
		// Route the aurora ribbons above and below the opening, rather than
		// through nonexistent center pixels. Both remain visible at silence.
		bend := ledBell(x, 1.0)
		w1 := .48*bend + .20 + .26*math.Sin(x*2.5+t*2.7) + .08*math.Cos(x*5-t*4)
		w2 := -.48*bend - .20 - .24*math.Cos(x*2.3-t*1.9)
		p1 := ledBell(y-w1, .24+.20*l)
		p2 := .8 * ledBell(y-w2, .24+.17*l)
		return ledMix(163, 284, p2/(p1+p2+.001)), .92 - .52*math.Min(p1, p2), .008 + (.35+.57*l)*p1 + (.31+.5*l)*p2
	}
	return 0, 0, 0
}

// Independent rounded HSV conversion for voice effects. Music retains the
// project's existing hsvToRGB implementation. Matches the preview exporter.
func ledVoiceColor(h, s, v float64) string {
	h = frac(h/360) * 6
	s = clamp01(s)
	v = clamp01(v)
	f := frac(h)
	p, q, u := v*(1-s), v*(1-s*f), v*(1-s*(1-f))
	rgb := [6][3]float64{{v, u, p}, {q, v, p}, {p, v, u}, {p, q, v}, {u, p, v}, {v, p, q}}[int(h)%6]
	return fmt.Sprintf("#%02X%02X%02X", int(math.Round(rgb[0]*255)), int(math.Round(rgb[1]*255)), int(math.Round(rgb[2]*255)))
}

// Oknix is a separate five-pixel indicator, synchronized to panel time
// and speech amplitude, not an extension of the 12x7 matrix.
func ledOknixPixel(mode ledMode, j int, t, l float64) (h, s, v float64) {
	x := (float64(j) - 2) / 2
	switch mode {
	case ledListen:
		pulse := ledSonarPulse(math.Abs(x)*1.65, t, 0, .30+.22*l)
		core := ledBell(x, .35+.55*l)
		return 186 + 14*math.Abs(x), .88, .018 + (.30+.42*l)*pulse + (.14+.28*l)*core
	case ledThink:
		pos := .95 * math.Cos(t*1.85)
		p1 := ledBell(x-pos, .42)
		p2 := ledBell(x+pos, .42)
		return ledMix(186, 269, p2/(p1+p2+.001)), .92 - .25*math.Max(p1, p2), .025 + .53*p1 + .44*p2
	case ledSpeak:
		p1 := .5 + .5*math.Sin(x*2.8+t*2.7)
		p2 := .5 + .5*math.Cos(x*2.5-t*1.9)
		return ledMix(163, 284, p2/(p1+p2+.001)), .9, .018 + (.13+.48*l)*p1 + (.09+.24*l)*p2
	}
	return 0, 0, 0
}

func ledBlackFrame() []string {
	out := make([]string, ledCount)
	for i := range out {
		out[i] = "#000000"
	}
	return out
}

func ledParseColor(s string) ([4]byte, error) {
	var c [4]byte
	if (len(s) != 7 && len(s) != 9) || s[0] != '#' {
		return c, fmt.Errorf("led: invalid color %q", s)
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return c, err
	}
	if len(s) == 7 {
		v <<= 8
	}
	return [4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}, nil
}

func ledBlendFrames(a, b []string, p float64) []string {
	p = clamp01(p)
	if p >= 1 {
		return b
	}
	out := make([]string, len(b))
	for i := range b {
		var x [4]byte
		if i < len(a) {
			x, _ = ledParseColor(a[i])
		}
		y, _ := ledParseColor(b[i])
		var c [4]byte
		for k := range c {
			c[k] = byte(math.Round(ledMix(float64(x[k]), float64(y[k]), p)))
		}
		out[i] = fmt.Sprintf("#%02X%02X%02X", c[0], c[1], c[2])
	}
	return out
}

// Keep the original project's framebuffer writer: it already knows the
// hardware byte order and any channel conversion. Do not bypass it.
func ledWriteNative(colors []string) ([]byte, error) {
	if ledCount != ledPixelCount || len(colors) != ledPixelCount {
		return nil, fmt.Errorf("led: expected %d pixels, ledCount=%d, frame=%d", ledPixelCount, ledCount, len(colors))
	}
	return writeLEDFramebufferBytes(colors)
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

type ledFrameWriter struct {
	last       []byte
	strictRead bool
	warnedRead bool
}

func (w *ledFrameWriter) write(colors []string, requireFree, watch bool) error {
	if requireFree || watch {
		cur, err := readLEDFramebuffer()
		if err != nil {
			if w.strictRead {
				return err
			}
			if !w.warnedRead {
				log.Printf("led: framebuffer readback unavailable: %v", err)
				w.warnedRead = true
			}
		} else if w.last == nil {
			if requireFree && !dimFrame(cur) {
				return errLEDForeign
			}
		} else if watch && !bytes.Equal(cur, w.last) {
			return errLEDForeign
		}
	}
	buf, err := ledWriteNative(colors)
	if err != nil {
		return err
	}
	w.last = buf
	return nil
}

func readLEDFramebuffer() ([]byte, error) {
	f, err := os.Open(ledFramebufferPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, ledPixelCount*4)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func dimFrame(buf []byte) bool {
	for i := 0; i+3 < len(buf); i += 4 {
		if max(buf[i], buf[i+1], buf[i+2]) > 64 {
			return false
		}
	}
	return true
}
