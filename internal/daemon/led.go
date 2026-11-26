package daemon

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/luna"
)

const (
	ledPatternDir    = "/system/vendor/quasar/ledpatterns/panel"
	ledListenPattern = ledPatternDir + "/alice_listening.led"
	ledCount         = 89
	ledFrameGap      = 20 * time.Millisecond
)

type ledFrame struct {
	colors []string
	delay  time.Duration
}

// loadLEDPattern reads a native Quasar panel animation. Each frame is a line
// of 84 RGBA colours followed by a delay in milliseconds.
func loadLEDPattern(path string) ([]ledFrame, error) {
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
			ir, ig, ib := invertRGB(uint8(r), uint8(g), uint8(b))
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

// invertRGB rotates hue by 180 degrees while keeping saturation and value.
// Black stays black, so the panel background does not become a white wall.
func invertRGB(r, g, b uint8) (uint8, uint8, uint8) {
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
	d := max - min
	var h float64
	switch max {
	case rf:
		h = (gf - bf) / d
		if gf < bf {
			h += 6
		}
	case gf:
		h = (bf-rf)/d + 2
	default:
		h = (rf-gf)/d + 4
	}
	h /= 6
	h += 0.5
	if h >= 1 {
		h -= 1
	}
	s := d / max
	v := max
	i := int(h * 6)
	f := h*6 - float64(i)
	p := v * (1 - s)
	q := v * (1 - s*f)
	t := v * (1 - s*(1-f))
	var nr, ng, nb float64
	switch i % 6 {
	case 0:
		nr, ng, nb = v, t, p
	case 1:
		nr, ng, nb = q, v, p
	case 2:
		nr, ng, nb = p, v, t
	case 3:
		nr, ng, nb = p, q, v
	case 4:
		nr, ng, nb = t, p, v
	default:
		nr, ng, nb = v, p, q
	}
	return uint8(nr*255 + 0.5), uint8(ng*255 + 0.5), uint8(nb*255 + 0.5)
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

// playLEDPattern loops the inverted native listening animation until cancel.
func (d *Daemon) playLEDPattern(ctx context.Context, lc *luna.Client, path string) {
	frames, err := loadLEDPattern(path)
	if err != nil {
		log.Printf("led: %v; using solid blue", err)
		_ = lc.SetRing(ctx, []string{"#2E7DFF"})
		return
	}
	for {
		for _, frame := range frames {
			if ctx.Err() != nil {
				return
			}
			if err := lc.SetRing(ctx, fitLEDColors(frame.colors, ledCount)); err != nil {
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
