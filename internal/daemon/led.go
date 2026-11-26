package daemon

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/luna"
)

const (
	ledPatternDir    = "/system/vendor/quasar/ledpatterns/panel"
	ledListenPattern = ledPatternDir + "/alice_listening.led"
	ledThinkPattern  = ledPatternDir + "/alice_thinking.led"
	ledSpeakPattern  = ledPatternDir + "/alice_speaking.led"
	ledCount         = 89
	ledFrameGap      = 20 * time.Millisecond
	ledHueListen     = 0.33 // green
	ledHueThink      = 0.60 // blue
	ledHueSpeak      = 0.08 // warm orange
	ledErrorColor    = "#FF2020"
	ledErrorDuration = 1500 * time.Millisecond
	ledErrorFrame    = 50 * time.Millisecond
)

type ledFrame struct {
	colors []string
	delay  time.Duration
}

// loadLEDPattern reads a native Quasar panel animation. Each frame is a line
// of 84 RGBA colours followed by a delay in milliseconds. hue replaces the
// native violet with a state colour while keeping saturation and brightness.
func loadLEDPattern(path string, hue float64) ([]ledFrame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	frames := make([]ledFrame, 0, 512)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		delay := ledFrameGap
		if n, err := strconv.Atoi(fields[len(fields)-1]); err == nil && n >= 0 && n <= 1000 {
			delay = time.Duration(n) * time.Millisecond
			fields = fields[:len(fields)-1]
		}
		colors := make([]string, 0, len(fields))
		for _, tok := range fields {
			if len(tok) < 6 {
				colors = nil
				break
			}
			r, err1 := strconv.ParseUint(tok[0:2], 16, 8)
			g, err2 := strconv.ParseUint(tok[2:4], 16, 8)
			b, err3 := strconv.ParseUint(tok[4:6], 16, 8)
			if err1 != nil || err2 != nil || err3 != nil {
				colors = nil
				break
			}
			ir, ig, ib := recolorRGB(uint8(r), uint8(g), uint8(b), hue)
			colors = append(colors, fmt.Sprintf("#%02X%02X%02X", ir, ig, ib))
		}
		if len(colors) == 0 {
			continue
		}
		frames = append(frames, ledFrame{colors: colors, delay: delay})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("no frames in %s", path)
	}
	return frames, nil
}

// recolorRGB replaces the hue while keeping saturation and value. Black stays
// black, so the panel background does not become a coloured wall.
func recolorRGB(r, g, b uint8, hue float64) (uint8, uint8, uint8) {
	rf, gf, bf := float64(r)/255, float64(g)/255, float64(b)/255
	max := rf
	if gf > max {
		max = gf
	}
	if bf > max {
		max = bf
	}
	min := rf
	if gf < min {
		min = gf
	}
	if bf < min {
		min = bf
	}
	if max == min {
		return r, g, b
	}
	return hsvToRGB(hue, (max-min)/max, max)
}

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

func hueColor(hue float64) string {
	r, g, b := hsvToRGB(hue, 0.85, 1)
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

func fitLEDColors(src []string, n int) []string {
	out := make([]string, n)
	for i := range out {
		j := i * len(src) / n
		if j >= len(src) {
			j = len(src) - 1
		}
		out[i] = src[j]
	}
	return out
}

type ledShow struct {
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// startLEDPattern runs a recoloured native animation until stop is called.
// level, when non-nil, is sampled for every frame and scales brightness, so
// the panel follows the microphone or the TTS voice instead of looping at a
// fixed brightness.
func (d *Daemon) startLEDPattern(ctx context.Context, lc *luna.Client, path string, hue float64, level func() float64) *ledShow {
	ledCtx, cancel := context.WithCancel(ctx)
	show := &ledShow{cancel: cancel, done: make(chan struct{})}
	log.Printf("led: start pattern hue=%.2f", hue)
	go func() {
		defer close(show.done)
		d.playLEDPattern(ledCtx, lc, path, hue, level)
	}()
	return show
}

func (s *ledShow) stop(_ *luna.Client) {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.cancel()
		<-s.done
		// Native maind can repaint between clear and Luna's ownership check,
		// so all Station 2 LED writes go straight to the RGBW framebuffer.
		clearLEDFramebuffer()
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
	if len(colors) == 0 {
		return nil
	}
	f, err := os.OpenFile("/dev/fb0", os.O_WRONLY, 0)
	if err != nil {
		return err
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
	_, err = f.Write(buf)
	return err
}

// pulseErrorLED shows one red heartbeat for 1.5 s and always turns the panel
// off. It is used for explicit failures and cancellations, not for silence
// (nobody spoke) and not for a normal end of the conversation.
func pulseErrorLED(ctx context.Context, lc *luna.Client) {
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

// playLEDPattern loops the recoloured native animation until cancel.
func (d *Daemon) playLEDPattern(ctx context.Context, lc *luna.Client, path string, hue float64, level func() float64) {
	clearLEDFramebuffer()
	frames, err := loadLEDPattern(path, hue)
	if err != nil {
		fallback := hueColor(hue)
		log.Printf("led: %v; using solid %s", err, fallback)
		for {
			if ctx.Err() != nil {
				return
			}
			_ = writeLEDFramebuffer(scaleLEDColors([]string{fallback}, ledLevel(level)))
			t := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
	for {
		for _, frame := range frames {
			if ctx.Err() != nil {
				return
			}
			colors := fitLEDColors(frame.colors, ledCount)
			colors = scaleLEDColors(colors, ledLevel(level))
			if err := writeLEDFramebuffer(colors); err != nil {
				log.Printf("led: write fb0: %v", err)
				return
			}
			t := time.NewTimer(frame.delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// ledLevel keeps a dim baseline in silence and lets the current voice or
// microphone level drive the rest of the brightness.
func ledLevel(level func() float64) float64 {
	if level == nil {
		return 1
	}
	v := level()
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return 0.25 + 0.75*v
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
