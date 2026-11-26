package daemon

import "time"

// muteDetection suppresses the wake-word detector while audio is played by
// this app (ack beep or Athena's answer) and for a short tail afterwards.
// Native Alice has the same protection through use-tts-blocker; we cannot
// touch her settings, so the guard lives entirely in our process.
func (d *Daemon) muteDetection(dur time.Duration) {
	if dur <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	until := time.Now().Add(dur)
	if until.After(d.muteUntil) {
		d.muteUntil = until
	}
	if d.det != nil {
		d.det.Reset()
		d.detMuted = true
	}
}

// currentChannel is the yio_audio channel the next connection should use.
func (d *Daemon) currentChannel() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.settings.Channel == "" {
		return "vqe_0"
	}
	return d.settings.Channel
}

// restartAudio closes the current maind subscription. Run() reconnects with
// the latest settings; used when the user switches vqe_0 <-> vqe_bnr.
func (d *Daemon) restartAudio() {
	d.audioMu.Lock()
	cancel := d.audioCancel
	d.audioMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
