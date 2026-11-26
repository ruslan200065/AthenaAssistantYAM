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
	"time"
)

// ledMode selects one of the app's own procedural animations. They replace the
// stock Quasar patterns for Athena turns, music and idle so the ring always has
// a coherent visual language of its own.
type ledMode int

const (
	ledListen ledMode = iota
	ledThink
	ledSpeak
	ledMusic
	ledIdle
)

const ledAnimFrame = 33 * time.Millisecond

var errLEDForeign = errors.New("led: another writer owns the framebuffer")

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

// startLEDAnimation starts a procedural animation. requireFree is used by the
// idle animation: it only starts if nobody else owns the framebuffer. watch
// then stops the animation as soon as another writer paints over it, so music
// and idle never fight native Alice or local.sendspin.
func (d *Daemon) startLEDAnimation(ctx context.Context, mode ledMode, level func() float64, requireFree, watch bool) *ledShow {
	ledCtx, cancel := context.WithCancel(ctx)
	show := &ledShow{
		cancel:      cancel,
		done:        make(chan struct{}),
		clearOnStop: mode != ledIdle,
		guardClear:  watch,
	}
	log.Printf("led: start %s", mode)
	go func() {
		defer close(show.done)
		d.playLEDAnimation(ledCtx, show, mode, level, requireFree, watch)
	}()
	return show
}

// playLEDAnimation renders frames at a fixed cadence from a pure function of
// elapsed time. No native pattern files are read or modified.
func (d *Daemon) playLEDAnimation(ctx context.Context, show *ledShow, mode ledMode, level func() float64, requireFree, watch bool) {
	if mode == ledListen || mode == ledThink || mode == ledSpeak {
		clearLEDFramebuffer()
	}
	start := time.Now()
	w := &ledFrameWriter{}
	for {
		if ctx.Err() != nil {
			return
		}
		lv := 0.0
		if level != nil {
			lv = level()
		}
		colors := animationFrame(mode, time.Since(start).Seconds(), lv)
		if err := w.write(colors, requireFree, watch); err != nil {
			if errors.Is(err, errLEDForeign) {
				log.Printf("led: %s yielded to another writer", mode)
				show.yielded.Store(true)
				return
			}
			log.Printf("led: %s: %v", mode, err)
			return
		}
		show.setFrame(w.last)
		t := time.NewTimer(ledAnimFrame)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// animationFrame returns one frame for all 89 LEDs. The ring is treated as a
// circle, so every pattern is seamless and has no visible start or end.
func animationFrame(mode ledMode, t, level float64) []string {
	out := make([]string, ledCount)
	level = clamp01(level)
	for i := range out {
		a := 2 * math.Pi * float64(i) / float64(ledCount)
		x := float64(i) / float64(ledCount)
		var hue, sat, val float64
		switch mode {
		case ledListen:
			w1 := 0.5 + 0.5*math.Sin(3*a-3.2*t)
			w2 := 0.5 + 0.5*math.Sin(5*a+1.7*t+1.3)
			hue = 0.35 + 0.07*w1 - 0.02*w2
			sat = 0.88
			val = 0.10 + 0.18*level + 0.34*w1*(0.25+0.75*level) + 0.18*w2*level
			if t < 1.1 {
				flash := 1 - t/1.1
				val += 0.62 * flash * flash
				sat -= 0.32 * flash
			}

		case ledThink:
			p1 := frac(t * 0.20)
			p2 := frac(p1 + 0.5)
			glow1 := math.Exp(-ringDist(x, p1)/0.045) + 0.32*math.Exp(-ringDist(x, p1)/0.16)
			glow2 := 0.60 * (math.Exp(-ringDist(x, p2)/0.040) + 0.28*math.Exp(-ringDist(x, p2)/0.13))
			star := math.Pow(math.Max(0, math.Sin(2*math.Pi*(x*7.3+t*0.9))), 18) * 0.45
			hue = 0.58 + 0.055*math.Sin(2*math.Pi*x-t*0.8)
			sat = 0.90
			val = 0.045 + 0.88*glow1 + glow2 + star

		case ledSpeak:
			w1 := 0.5 + 0.5*math.Sin(7*a+7*t)
			w2 := 0.5 + 0.5*math.Sin(3*a-4*t+1.0)
			hue = 0.055 + 0.05*w2
			sat = 0.86
			val = 0.09 + 0.16*level + 0.38*w1*(0.28+0.72*level) + 0.14*w2

		case ledMusic:
			beat := math.Exp(-7 * frac(t*1.9))
			w1 := 0.5 + 0.5*math.Sin(6*a-5*t)
			w2 := 0.5 + 0.5*math.Sin(3*a+2.2*t)
			spark := math.Pow(math.Max(0, math.Sin(2*math.Pi*(x*5.7+t*1.25))), 22)
			hue = frac(x + 0.08*t)
			sat = 0.92
			val = 0.10 + 0.20*w1 + 0.26*beat + 0.18*w1*w2 + 0.45*spark*(0.35+beat)

		default: // ledIdle: a calm, very dim breathing aurora
			w1 := 0.5 + 0.5*math.Sin(2*a+0.7*t)
			w2 := 0.5 + 0.5*math.Sin(4*a-0.45*t+1.8)
			hue = 0.56 + 0.035*math.Sin(0.25*t)
			sat = 0.80
			val = 0.070 + 0.090*w1 + 0.050*w2
		}
		out[i] = hsvString(hue, clamp01(sat), clamp01(val))
	}
	return out
}

func ringDist(a, b float64) float64 {
	d := math.Abs(a - b)
	return math.Min(d, 1-d)
}

func frac(v float64) float64 {
	return v - math.Floor(v)
}

func clamp01(v float64) float64 {
	if v < 0 {
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

// ledFrameWriter remembers the last frame it painted. When watch is set, a
// different framebuffer means somebody else (native Alice, a volume overlay,
// local.sendspin) took over, and the animation yields instead of fighting it.
type ledFrameWriter struct {
	last []byte
}

func (w *ledFrameWriter) write(colors []string, requireFree, watch bool) error {
	if requireFree || watch {
		cur, err := readLEDFramebuffer()
		if err == nil {
			if w.last == nil {
				// Native firmware keeps a very dim idle glow on the ring. It is
				// safe to replace that, but a bright frame means Alice,
				// music or a notification is using the LEDs right now.
				if requireFree && !dimFrame(cur) {
					return errLEDForeign
				}
			} else if watch && !bytes.Equal(cur, w.last) {
				return errLEDForeign
			}
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

// dimFrame accepts a black or very dim framebuffer, which is what the station
// shows when it is idle. Anything brighter belongs to an active indicator.
func dimFrame(buf []byte) bool {
	for i := 0; i+2 < len(buf); i += 4 {
		if max(buf[i], buf[i+1], buf[i+2]) > 64 {
			return false
		}
	}
	return true
}
