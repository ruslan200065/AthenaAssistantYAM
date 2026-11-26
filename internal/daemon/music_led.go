package daemon

import (
	"context"
	"log"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/luna"
)

// beginAthenaLED claims the ring for a dialogue turn. Music and idle
// animations are stopped and cleared, and they cannot restart until
// endAthenaLED releases the ring.
func (d *Daemon) beginAthenaLED() {
	d.ledMu.Lock()
	defer d.ledMu.Unlock()
	d.ledAthena = true
	d.stopAuxLEDLocked()
}

func (d *Daemon) endAthenaLED() {
	d.ledMu.Lock()
	d.ledAthena = false
	d.auxRetryAfter = time.Time{}
	d.ledMu.Unlock()
}

func (d *Daemon) stopAuxLEDLocked() {
	if d.auxShow == nil {
		return
	}
	d.auxShow.stop(nil)
	d.auxShow = nil
	d.auxMode = -1
}

// auxLEDLoop keeps a visual on the ring whenever Athena is quiet:
// local.sendspin playing -> music spectrum, otherwise a calm idle aurora.
// The idle animation only starts on an unowned (black) framebuffer and yields
// the moment native Alice or another indicator paints, so it never fights the
// station's own lights.
func (d *Daemon) auxLEDLoop(ctx context.Context) {
	const poll = 1500 * time.Millisecond
	for {
		d.updateAuxLED(ctx)
		select {
		case <-ctx.Done():
			d.ledMu.Lock()
			d.stopAuxLEDLocked()
			d.ledMu.Unlock()
			return
		case <-time.After(poll):
		}
	}
}

func (d *Daemon) updateAuxLED(ctx context.Context) {
	cfg := d.athenaSettings()
	if !cfg.LEDFeedback {
		d.ledMu.Lock()
		d.stopAuxLEDLocked()
		d.ledMu.Unlock()
		return
	}

	playing := false
	if cfg.LunaURL != "" {
		mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := luna.New(cfg.LunaURL).Media(mctx)
		cancel()
		if err != nil {
			log.Printf("led: media state: %v", err)
		} else {
			playing = st.Playing()
		}
	}

	d.ledMu.Lock()
	defer d.ledMu.Unlock()
	if d.ledAthena {
		d.stopAuxLEDLocked()
		return
	}
	if d.auxShow != nil && showDone(d.auxShow) {
		if d.auxShow.yielded.Load() {
			// Another indicator took over. Do not blink between its frames:
			// wait until the dialog or notification is likely finished.
			d.auxRetryAfter = time.Now().Add(6 * time.Second)
		}
		d.auxShow = nil
		d.auxMode = -1
	}
	if time.Now().Before(d.auxRetryAfter) {
		return
	}
	want := ledIdle
	requireFree, watch := true, true
	if playing {
		want = ledMusic
	}
	if d.auxShow != nil && d.auxMode == want {
		return
	}
	d.stopAuxLEDLocked()
	d.auxMode = want
	d.auxShow = d.startLEDAnimation(ctx, want, nil, requireFree, watch)
}

func showDone(s *ledShow) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
