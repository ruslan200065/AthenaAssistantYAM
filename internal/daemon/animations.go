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
	ledPanelWidth      = 12
	ledPanelHeight     = 7
	ledPanelCount      = ledPanelWidth * ledPanelHeight
	ledOknixCount      = 5
	ledPixelCount      = ledPanelCount + ledOknixCount
	ledPanelSerpentine = false
	ledPanelFlipX      = false
	ledPanelFlipY      = false
	ledOknixReverse    = false
	ledFramebufferPath = "/dev/fb0"
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

// animationFrame renders the two native elements in a single framebuffer:
// panel (84 pixels, 12x7) followed by oknix (5 pixels).
// Native voice patterns keep the fourth byte zero; match them by default.
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
	for row := 0; row < ledPanelHeight; row++ {
		for col := 0; col < ledPanelWidth; col++ {
			x, y := (float64(col)-5.5)/3.6, (float64(row)-3)/3.6
			h, s, v := ledPanelPixel(mode, x, y, t, level)
			out[ledPanelIndex(col, row)] = hsvString(frac(h/360), clamp01(s), clamp01(v))
		}
	}
	for j := 0; j < ledOknixCount; j++ {
		h, s, v := ledOknixPixel(mode, j, t, level)
		k := j
		if ledOknixReverse {
			k = ledOknixCount - 1 - j
		}
		out[ledPanelCount+k] = hsvString(frac(h/360), clamp01(s), clamp01(v))
	}
	return out
}

// Logical panel coordinates -> physical framebuffer index.
func ledPanelIndex(col, row int) int {
	if ledPanelFlipX {
		col = ledPanelWidth - 1 - col
	}
	if ledPanelFlipY {
		row = ledPanelHeight - 1 - row
	}
	if ledPanelSerpentine && row%2 == 1 {
		col = ledPanelWidth - 1 - col
	}
	return row*ledPanelWidth + col
}

// Pulse envelope smoothly hides the radius reset between waves.
func ledSonarPulse(r, t, offset, width float64) float64 {
	p := frac(t*.65 + offset)
	env := ledSmoothstep(p/.07) * ledSmoothstep((1-p)/.10)
	return env * ledBell(r-p*2.1, width)
}

func ledPanelPixel(mode ledMode, x, y, t, l float64) (h, s, v float64) {
	r := math.Hypot(x, y)
	switch mode {
	case ledListen: // L1: radial sonar, single startup flash.
		ring := ledSonarPulse(r, t, 0, .19+.12*l)
		ring2 := .45 * ledSonarPulse(r, t, .55, .17)
		core := ledBell(r, .38+.22*l)
		flash := ledBell(t-.32, .23)
		return 184 + 22*clamp01(r/2), .9 - .62*flash, .025 + (.48+.5*l)*ring + ring2 + core*(.15+.38*l) + .65*flash
	case ledThink: // T1: two elliptical orbiting comets with tapered tails.
		p1, p2 := 0.0, 0.0
		q := t * 1.85
		for k := 0; k < 9; k++ {
			u := q - float64(k)*.17
			w := math.Pow(1-float64(k)/10, 2)
			cx, cy := 1.25*math.Cos(u), .68*math.Sin(u)
			p1 = math.Max(p1, w*ledBell(math.Hypot(x-cx, y-cy), .27))
			p2 = math.Max(p2, w*ledBell(math.Hypot(x+cx, y+cy), .27))
		}
		path := ledBell(math.Hypot(x/1.25, y/.68)-1, .12)
		return ledMix(186, 269, p2/(p1+p2+.001)), .95 - .56*math.Max(p1, p2), .018 + .11*path + .98*p1 + .86*p2 + .12*ledBell(r, .25)
	case ledSpeak: // S3: two reactive aurora ribbons.
		w1 := .42*math.Sin(x*2.5+t*2.7) + .10*math.Cos(x*5-t*4)
		w2 := -.32*math.Cos(x*2.3-t*1.9) - .23
		p1 := ledBell(y-w1, .18+.22*l)
		p2 := .8 * ledBell(y-w2, .20+.13*l)
		return ledMix(163, 284, p2/(p1+p2+.001)), .92 - .52*math.Min(p1, p2), .018 + (.35+.57*l)*p1 + (.31+.5*l)*p2
	}
	return 0, 0, 0
}

// Oknix is a separate five-pixel indicator, synchronized to panel time
// and speech amplitude, not an extension of the 12x7 matrix.
func ledOknixPixel(mode ledMode, j int, t, l float64) (h, s, v float64) {
	x := (float64(j) - 2) / 2
	switch mode {
	case ledListen:
		flash := ledBell(t-.32, .23)
		pulse := ledSonarPulse(math.Abs(x)*1.65, t, 0, .30+.22*l)
		core := ledBell(x, .35+.55*l)
		return 186 + 14*math.Abs(x), .88 - .60*flash, .018 + (.30+.42*l)*pulse + (.14+.28*l)*core + .48*flash
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
		out[i] = "#00000000"
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
		out[i] = fmt.Sprintf("#%02X%02X%02X%02X", c[0], c[1], c[2], c[3])
	}
	return out
}

// Write the four-byte native color layout. Voice effects use RGB + zero
// fourth byte, matching native voice pattern files; its hardware semantics
// need not be assumed.
func ledEncodeRGBW(colors []string) ([]byte, error) {
	if ledCount != ledPixelCount || len(colors) != ledPixelCount {
		return nil, fmt.Errorf("led: expected %d pixels, ledCount=%d, frame=%d", ledPixelCount, ledCount, len(colors))
	}
	buf := make([]byte, ledPixelCount*4)
	for i, s := range colors {
		c, err := ledParseColor(s)
		if err != nil {
			return nil, err
		}
		copy(buf[4*i:4*i+4], c[:])
	}
	return buf, nil
}
func ledWriteRGBW(colors []string) ([]byte, error) {
	buf, err := ledEncodeRGBW(colors)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(ledFramebufferPath, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	n, err := f.Write(buf)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if n != len(buf) {
		return nil, io.ErrShortWrite
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return buf, nil
}

func ledBell(x, s float64) float64    { return math.Exp(-math.Pow(x/s, 2)) }
func ledMix(a, b, p float64) float64  { return a + (b-a)*p }
func ledSmoothstep(x float64) float64 { x = clamp01(x); return x * x * (3 - 2*x) }
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
	buf, err := ledWriteRGBW(colors)
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
		if max(buf[i], buf[i+1], buf[i+2], buf[i+3]) > 64 {
			return false
		}
	}
	return true
}
