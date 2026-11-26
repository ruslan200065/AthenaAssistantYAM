package daemon

import (
	"context"
	"log"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/luna"
)

// beginAthenaLED claims the ring for a dialogue turn. The music animation
// is stopped and cleared, and it cannot restart until endAthenaLED releases
// the ring.
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

// auxLEDLoop keeps the music animation on the ring while local.sendspin is
// playing. When nothing is playing the ring is not touched at all.
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
	if !playing {
		d.stopAuxLEDLocked()
		return
	}
	if d.auxShow != nil && d.auxMode == ledMusic {
		return
	}
	d.stopAuxLEDLocked()
	d.auxMode = ledMusic
	d.auxShow = d.startLEDAnimation(ctx, ledMusic, nil, true, true)
}

func showDone(s *ledShow) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
