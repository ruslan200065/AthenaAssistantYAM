package daemon

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/athena"
	"github.com/ruslan200065/AthenaAssistantYAM/internal/gateway"
	"github.com/ruslan200065/AthenaAssistantYAM/internal/luna"
	"github.com/ruslan200065/AthenaAssistantYAM/internal/wav"
)

var errAthenaBusy = errors.New("athena: предыдущий запрос ещё выполняется")

// athenaSettings returns a snapshot safe to use outside d.mu.
func (d *Daemon) athenaSettings() Settings {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.settings
}

// runAthena handles one wake-word hit without touching native Alice. The
// first turn is a normal wake turn; every reply with listen=true opens a
// follow-up window on the same gateway session, with the stock listen sound
// and the LED animation again, until the core ends the dialogue.
func (d *Daemon) runAthena(ctx context.Context) error {
	if !d.athenaMu.TryLock() {
		return errAthenaBusy
	}
	defer d.athenaMu.Unlock()

	cfg := d.athenaSettings()
	lc := luna.New(cfg.LunaURL)

	// Athena owns the ring for the whole turn: the music animation yields at
	// once, and the panel is cleared before they are allowed back.
	d.beginAthenaLED()
	defer d.endAthenaLED()

	// Never leave the matrix lit when the turn exits for any reason.
	defer func() {
		if cfg.LEDFeedback {
			clearLEDFramebuffer()
		}
	}()

	// The LED context is independent of a single turn: it is stopped
	// explicitly before TTS, restarted for every follow-up, and cancelled on
	// every exit path.
	ledCtx, ledCancel := context.WithCancel(ctx)
	defer ledCancel()

	var listenShow, thinkShow *ledShow
	startListening := func() {
		if !cfg.LEDFeedback {
			return
		}
		listenShow = d.startLEDAnimation(ledCtx, ledListen, d.micLevelValue, false, false)
	}
	stopListening := func() {
		listenShow.stop(lc)
		listenShow = nil
	}
	startThinking := func() {
		if !cfg.LEDFeedback {
			return
		}
		thinkShow = d.startLEDAnimation(ledCtx, ledThink, nil, false, false)
	}
	stopThinking := func() {
		thinkShow.stop(lc)
		thinkShow = nil
	}
	defer stopListening()
	defer stopThinking()

	gw := gateway.New(cfg.AthenaURL, cfg.AthenaToken)
	sess, err := gw.Open(ctx)
	if err != nil {
		d.athenaError(ctx, lc, cfg)
		return err
	}
	defer sess.Close()

	startListening()
	d.playSound(ctx, lc, "wake")

	// Open the utterance before capturing: listen.ack carries
	// wait_for_speech_seconds, so the microphone stays open exactly as long as
	// the core expects instead of an arbitrary local 4 seconds.
	ack, err := sess.BeginTurn(ctx, false, 0)
	if err != nil {
		if isWakeLost(err) {
			d.wakeLost(ctx, cfg)
			return nil
		}
		d.athenaError(ctx, lc, cfg)
		return err
	}
	waitFor := time.Duration(ack.WaitForSpeechSeconds * float64(time.Second))
	if waitFor <= 0 {
		waitFor = 10 * time.Second
	}
	maxSpeech := time.Duration(cfg.RecognizeTimeoutSeconds) * time.Second
	if maxSpeech < 10*time.Second {
		maxSpeech = 10 * time.Second
	}
	if maxSpeech > 30*time.Second {
		maxSpeech = 30 * time.Second
	}
	maxCmd := wakeSettle + waitFor + maxSpeech
	log.Printf("athena: waiting up to %.0f s for speech (ack)", waitFor.Seconds())

	pcm, err := d.captureCommand(ctx, maxCmd, wakeSettle, waitFor, d.setMicLevel)
	stopListening()
	if err != nil {
		_ = sess.AbortTurn(context.Background())
		d.athenaError(ctx, lc, cfg)
		return err
	}
	if len(pcm) == 0 {
		// Nobody spoke. This is not an error: close the empty utterance, do not
		// play the error sound and do not flash red.
		log.Printf("athena: no speech recognized")
		_ = sess.AbortTurn(context.Background())
		_ = sess.EndConversation(context.Background(), 0)
		return nil
	}
	log.Printf("athena: captured %.1f s, sending to gateway", float64(len(pcm))/float64(16000))

	startThinking()
	reply, err := sess.SendTurn(ctx, pcm)
	stopThinking()
	if err != nil {
		if isWakeLost(err) {
			d.wakeLost(ctx, cfg)
			return nil
		}
		d.athenaError(ctx, lc, cfg)
		return err
	}
	if reply.Early {
		// The core closed the dialogue before answering. This is a cancellation
		// (another station answered, intercom), not normal silence.
		if reply.Reason == "wake_lost" {
			d.wakeLost(ctx, cfg)
			return nil
		}
		log.Printf("athena: core ended dialogue early: %s", reply.Reason)
		d.athenaError(ctx, lc, cfg)
		return nil
	}

	for {
		log.Printf("athena: heard request, reply %q (listen=%v end=%v)", reply.Text, reply.Listen, reply.End)
		if strings.TrimSpace(reply.Text) != "" {
			if _, err := d.speakReply(ctx, cfg, reply.Text); err != nil {
				d.athenaError(ctx, lc, cfg)
				return err
			}
		}
		_ = sess.PlaybackStopped(ctx)

		keepListening := reply.Listen && !reply.End
		newTurn := false
		if !keepListening && wantsFollowUp(reply.Text) {
			// The model sometimes ends with an invitation or a question and
			// [end]. The user hears a prompt and expects to answer it, so keep
			// the microphone open. If the core already ended the conversation,
			// open a fresh wake turn instead of a rejected follow-up.
			log.Printf("athena: reply invites an answer; opening listening turn")
			keepListening = true
			newTurn = reply.End
		}
		if !keepListening {
			_ = sess.EndConversation(ctx, reply.ConversationID)
			return nil
		}

		convID := reply.ConversationID
		startListening()
		listenFor := d.playSound(ctx, lc, "listen")

		var ack *gateway.Ack
		if newTurn {
			ack, err = sess.BeginTurn(ctx, false, 0)
		} else {
			ack, err = sess.BeginTurn(ctx, true, convID)
		}
		if err != nil {
			stopListening()
			if isWakeLost(err) {
				d.wakeLost(ctx, cfg)
				return nil
			}
			d.athenaError(ctx, lc, cfg)
			_ = sess.EndConversation(context.Background(), convID)
			return nil
		}
		waitFor := time.Duration(ack.WaitForSpeechSeconds * float64(time.Second))
		if waitFor <= 0 {
			waitFor = 10 * time.Second
		}
		maxSpeech := time.Duration(cfg.RecognizeTimeoutSeconds) * time.Second
		if maxSpeech < 10*time.Second {
			maxSpeech = 10 * time.Second
		}
		if maxSpeech > 30*time.Second {
			maxSpeech = 30 * time.Second
		}
		maxCmd := settleFor(listenFor) + waitFor + maxSpeech
		log.Printf("athena: listening for follow-up (%.0f s, ack)", waitFor.Seconds())

		settle := settleFor(listenFor)
		pcm, err = d.captureCommand(ctx, maxCmd, settle, waitFor, d.setMicLevel)
		stopListening()
		if err != nil {
			_ = sess.AbortTurn(context.Background())
			if ctx.Err() == nil {
				log.Printf("athena: follow-up capture: %v", err)
				d.athenaError(ctx, lc, cfg)
			}
			_ = sess.EndConversation(context.Background(), convID)
			return nil
		}
		if len(pcm) == 0 {
			log.Printf("athena: follow-up window closed, nobody spoke")
			_ = sess.AbortTurn(context.Background())
			_ = sess.EndConversation(context.Background(), convID)
			return nil
		}
		log.Printf("athena: captured follow-up %.1f s", float64(len(pcm))/float64(16000))
		startThinking()
		reply, err = sess.SendTurn(ctx, pcm)
		stopThinking()
		if err != nil {
			if isWakeLost(err) {
				d.wakeLost(ctx, cfg)
				return nil
			}
			var drop *gateway.DropError
			if errors.As(err, &drop) {
				log.Printf("athena: follow-up dropped: %s", drop.Reason)
			} else {
				log.Printf("athena: follow-up: %v", err)
			}
			d.athenaError(ctx, lc, cfg)
			_ = sess.EndConversation(context.Background(), convID)
			return nil
		}
	}
}

// wantsFollowUp recognises answers that ask the person to continue even when
// the core marked the turn as ended with [end].
func wantsFollowUp(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if strings.HasSuffix(t, "?") {
		return true
	}
	for _, phrase := range []string{
		"говорите", "скажите", "слушаю", "я здесь",
		"чем могу помочь", "что нужно", "что-нибудь ещё", "продолжайте",
	} {
		if strings.Contains(t, phrase) {
			return true
		}
	}
	return false
}

// isWakeLost reports that another station claimed the wake word. This is a
// normal handoff, not an error: the app closes quietly and shows an upward
// animation instead of the red error heartbeat.
func isWakeLost(err error) bool {
	var drop *gateway.DropError
	return errors.As(err, &drop) && drop.Reason == "wake_lost"
}

// wakeLost hands the request over to the other station without an error sound.
func (d *Daemon) wakeLost(ctx context.Context, cfg Settings) {
	log.Printf("athena: wake_lost: another station accepted the request, handing off")
	if cfg.LEDFeedback {
		d.showWakeLostLED(ctx)
	}
}

// athenaError reports an explicit failure or rejection: the stock error sound
// plus one red heartbeat for 1.5 s, then the panel is turned off. Silence is
// handled separately and must not use this path.
func (d *Daemon) athenaError(ctx context.Context, lc *luna.Client, cfg Settings) {
	d.playSound(context.Background(), lc, "error")
	if cfg.LEDFeedback {
		pulseErrorLED(ctx, lc)
	}
}

// AskAthena is used by the test endpoint: it bypasses wake-word recognition.
func (d *Daemon) AskAthena(ctx context.Context, text string) error {
	if !d.athenaMu.TryLock() {
		return errAthenaBusy
	}
	defer d.athenaMu.Unlock()
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("athena: пустой текст")
	}
	return d.askAthena(ctx, d.athenaSettings(), text)
}

func (d *Daemon) askAthena(ctx context.Context, cfg Settings, text string) error {
	if cfg.AthenaURL == "" || cfg.AthenaToken == "" {
		return errors.New("athena: укажите URL и токен устройства")
	}
	ac := athena.New(cfg.AthenaURL, cfg.AthenaToken)
	resp, err := ac.Ask(ctx, text)
	if err != nil {
		return err
	}
	if strings.TrimSpace(resp.Reply) == "" {
		log.Printf("athena: empty reply (conversation %d)", resp.ConversationID)
		return nil
	}
	log.Printf("athena: reply %q (%.0f ms)", resp.Reply, float64(resp.LatencyMS))
	_, err = d.speakReply(ctx, cfg, resp.Reply)
	return err
}

// speakReply synthesizes the answer, runs the inverted speaking animation for
// the real playback duration, and waits for Luna to finish playing. The
// caller can then send playback.stopped to the core and open a follow-up.
func (d *Daemon) speakReply(ctx context.Context, cfg Settings, reply string) (time.Duration, error) {
	if cfg.MuteReplies {
		return 0, nil
	}
	ac := athena.New(cfg.AthenaURL, cfg.AthenaToken)
	pcm, rate, err := ac.TTS(ctx, reply, cfg.TTSVoice)
	if err != nil {
		return 0, err
	}
	if len(pcm) == 0 {
		return 0, errors.New("athena: tts returned no audio")
	}
	playFor := time.Duration(len(pcm)) * time.Second / time.Duration(rate)
	d.muteDetection(playFor + time.Second)
	lc := luna.New(cfg.LunaURL)

	var show *ledShow
	if cfg.LEDFeedback {
		show = d.startLEDAnimation(ctx, ledSpeak, speechLevel(pcm, rate), false, false)
	}
	defer show.stop(lc)

	if err := lc.PlayWAV(ctx, wav.Encode(pcm, rate)); err != nil {
		return 0, err
	}
	log.Printf("athena: playing %d Hz, %.1f s", rate, float64(len(pcm))/float64(rate))
	select {
	case <-time.After(playFor + 500*time.Millisecond):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return playFor, nil
}

func (d *Daemon) athenaConfigView() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{
		"athena_url":                d.settings.AthenaURL,
		"athena_token_set":          d.settings.AthenaToken != "",
		"luna_url":                  d.settings.LunaURL,
		"channel":                   d.settings.Channel,
		"recognize_timeout_seconds": d.settings.RecognizeTimeoutSeconds,
		"tts_voice":                 d.settings.TTSVoice,
		"mute_replies":              d.settings.MuteReplies,
	}
}

func (d *Daemon) athenaHealth(ctx context.Context) map[string]any {
	cfg := d.athenaSettings()
	out := map[string]any{
		"athena_url":  cfg.AthenaURL,
		"luna_url":    cfg.LunaURL,
		"configured":  cfg.AthenaURL != "" && cfg.AthenaToken != "",
		"recognize_s": cfg.RecognizeTimeoutSeconds,
	}
	if cfg.AthenaURL != "" && cfg.AthenaToken != "" {
		ac := athena.New(cfg.AthenaURL, cfg.AthenaToken)
		resp, err := ac.Health(ctx)
		if err != nil {
			out["athena_error"] = err.Error()
		} else {
			out["athena"] = resp
		}
	}
	if cfg.LunaURL != "" {
		code, err := luna.New(cfg.LunaURL).Health(ctx)
		if err != nil {
			out["luna_error"] = err.Error()
		} else {
			out["luna_status"] = code
		}
	}
	return out
}
