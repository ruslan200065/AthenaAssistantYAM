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

	// Never leave the matrix lit when the turn exits for any reason.
	defer func() {
		if cfg.LEDFeedback {
			_ = lc.RingOff(context.Background())
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
		listenShow = d.startLEDPattern(ledCtx, lc, ledListenPattern, ledHueListen, d.micLevelValue)
	}
	stopListening := func() {
		listenShow.stop(lc)
		listenShow = nil
	}
	startThinking := func() {
		if !cfg.LEDFeedback {
			return
		}
		thinkShow = d.startLEDPattern(ledCtx, lc, ledThinkPattern, ledHueThink, nil)
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

	maxCmd := time.Duration(cfg.RecognizeTimeoutSeconds) * time.Second
	if maxCmd < 5*time.Second {
		maxCmd = 5 * time.Second
	}
	if maxCmd > 20*time.Second {
		maxCmd = 20 * time.Second
	}
	pcm, err := d.captureCommand(ctx, maxCmd, wakeSettle, 4*time.Second, d.setMicLevel)
	stopListening()
	if err != nil {
		d.athenaError(ctx, lc, cfg)
		return err
	}
	if len(pcm) == 0 {
		// Nobody spoke. This is not an error: do not play the error sound and
		// do not flash red, just let the deferred cleanup turn the panel off.
		log.Printf("athena: no speech recognized")
		return nil
	}
	log.Printf("athena: captured %.1f s, sending to gateway", float64(len(pcm))/float64(16000))

	startThinking()
	reply, err := sess.Turn(ctx, pcm, false, 0)
	stopThinking()
	if err != nil {
		d.athenaError(ctx, lc, cfg)
		return err
	}
	if reply.Early {
		// The core closed the dialogue before answering. This is a cancellation
		// (another station answered, intercom, wake lost), not normal silence.
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
		if !keepListening && strings.HasSuffix(strings.TrimSpace(reply.Text), "?") {
			// The core prompt asks the model to end question replies with
			// [listen], but a model sometimes answers with a question and
			// [end]. The user hears a question and expects to answer it, so
			// open one follow-up window anyway; the core accepts follow_up
			// with the same conversation id.
			log.Printf("athena: reply is a question; opening follow-up despite listen=false")
			keepListening = true
		}
		if !keepListening {
			_ = sess.EndConversation(ctx, reply.ConversationID)
			return nil
		}

		followFor := time.Duration(reply.FollowUpTimeoutSeconds) * time.Second
		if followFor <= 0 {
			followFor = 8 * time.Second
		}
		if followFor < 3*time.Second {
			followFor = 3 * time.Second
		}
		if followFor > 30*time.Second {
			followFor = 30 * time.Second
		}
		log.Printf("athena: listening for follow-up (%.0f s)", followFor.Seconds())

		startListening()
		listenFor := d.playSound(ctx, lc, "listen")
		settle := listenFor + 100*time.Millisecond
		if settle < 300*time.Millisecond {
			settle = 300 * time.Millisecond
		}
		pcm, err = d.captureCommand(ctx, followFor, settle, followFor, d.setMicLevel)
		stopListening()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("athena: follow-up capture: %v", err)
				d.athenaError(ctx, lc, cfg)
			}
			_ = sess.EndConversation(context.Background(), reply.ConversationID)
			return nil
		}
		if len(pcm) == 0 {
			log.Printf("athena: follow-up window closed, nobody spoke")
			_ = sess.EndConversation(context.Background(), reply.ConversationID)
			return nil
		}
		log.Printf("athena: captured follow-up %.1f s", float64(len(pcm))/float64(16000))
		convID := reply.ConversationID
		startThinking()
		reply, err = sess.Turn(ctx, pcm, true, convID)
		stopThinking()
		if err != nil {
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
		show = d.startLEDPattern(ctx, lc, ledSpeakPattern, ledHueSpeak, speechLevel(pcm, rate))
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
