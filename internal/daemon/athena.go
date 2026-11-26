package daemon

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/athena"
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

// runAthena handles one wake-word hit without touching native Alice: Luna
// performs recognition-only capture, Athena answers, and Luna plays the WAV.
func (d *Daemon) runAthena(ctx context.Context) error {
	if !d.athenaMu.TryLock() {
		return errAthenaBusy
	}
	defer d.athenaMu.Unlock()

	cfg := d.athenaSettings()
	lc := luna.New(cfg.LunaURL)
	if cfg.LEDFeedback {
		if err := lc.SetRing(ctx, []string{"#2E7DFF"}); err == nil {
			defer lc.RingOff(context.Background())
		}
	}
	timeout := time.Duration(cfg.RecognizeTimeoutSeconds) * time.Second
	text, err := lc.Recognize(ctx, timeout)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		log.Printf("athena: no speech recognized")
		return nil
	}
	log.Printf("athena: heard %q", text)
	return d.askAthena(ctx, cfg, text)
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
	if cfg.MuteReplies {
		return nil
	}
	pcm, rate, err := ac.TTS(ctx, resp.Reply, cfg.TTSVoice)
	if err != nil {
		return err
	}
	if len(pcm) == 0 {
		return errors.New("athena: tts returned no audio")
	}
	lc := luna.New(cfg.LunaURL)
	if err := lc.PlayWAV(ctx, wav.Encode(pcm, rate)); err != nil {
		return err
	}
	log.Printf("athena: playing %d Hz, %.1f s", rate, float64(len(pcm))/float64(rate))
	return nil
}

func (d *Daemon) athenaConfigView() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{
		"athena_url":                d.settings.AthenaURL,
		"athena_token_set":          d.settings.AthenaToken != "",
		"luna_url":                  d.settings.LunaURL,
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
