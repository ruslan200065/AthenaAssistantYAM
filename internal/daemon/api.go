package daemon

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/kws"
	"github.com/ruslan200065/AthenaAssistantYAM/internal/wav"
)

//go:embed ui/index.html
var indexHTML []byte

type wordView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	Sensitivity float32   `json:"sensitivity"`
	Threshold   float32   `json:"threshold"`
	Recall      float64   `json:"recall"`
	FalsePerH   float64   `json:"false_per_h"`
	Takes       int       `json:"takes"`
	FalseClips  int       `json:"false_clips"`
	Created     time.Time `json:"created"`
	Score       float32   `json:"score"`
	Actions     []string  `json:"actions,omitempty"`
	Notes       []string  `json:"notes,omitempty"`
}

type enrollView struct {
	Name       string    `json:"name"`
	Takes      []float64 `json:"takes"` // detected word length per take
	HasAmbient bool      `json:"has_ambient"`
	SpeechSecs float64   `json:"speech_secs"`
	MinTakes   int       `json:"min_takes"`
}

type stateView struct {
	Station string `json:"station"`
	Audio   struct {
		OK      bool    `json:"ok"`
		Error   string  `json:"error,omitempty"`
		Level   float64 `json:"level"`
		Channel string  `json:"channel"`
		Muted   bool    `json:"muted"`
	} `json:"audio"`
	Words  []wordView  `json:"words"`
	Hits   []*Hit      `json:"hits"`
	Enroll *enrollView `json:"enroll"`
	Train  trainState  `json:"train"`
}

func (d *Daemon) state() stateView {
	d.mu.Lock()
	defer d.mu.Unlock()
	var v stateView
	v.Station = d.stationName()
	v.Audio.OK, v.Audio.Error, v.Audio.Level, v.Audio.Channel = d.audioOK, d.audioErr, d.level, d.settings.Channel
	v.Audio.Muted = d.detMuted || time.Now().Before(d.muteUntil)
	v.Words = make([]wordView, 0, len(d.words)) // [] not null for the UI
	scores := map[string]float32{}
	if d.det != nil {
		for i, id := range d.detWords {
			scores[id] = d.det.Scores[i]
		}
	}
	for _, w := range d.words {
		s := d.settings.Words[w.ID]
		falses, _ := filepath.Glob(filepath.Join(d.wordDir(w.ID), "false-*.wav"))
		v.Words = append(v.Words, wordView{
			ID: w.ID, Name: w.Model.Name, Enabled: s != nil && s.Enabled, Sensitivity: s.Sensitivity,
			Threshold: w.Model.Threshold, Recall: w.Model.Recall, FalsePerH: w.Model.FalsePerH,
			Takes: w.Model.Takes, FalseClips: len(falses), Created: w.Model.Created,
			Score: scores[w.ID], Notes: w.Model.TrainNotes, Actions: d.actionsFor(w.ID),
		})
	}
	v.Hits = make([]*Hit, len(d.hits))
	for i, h := range d.hits {
		v.Hits[len(d.hits)-1-i] = h // newest first
	}
	if e := d.enroll; e != nil {
		ev := &enrollView{Name: e.Name, HasAmbient: len(e.Ambient) > 0, MinTakes: minTakes,
			SpeechSecs: float64(len(e.Speech)) / kws.SampleRate, Takes: []float64{}}
		for _, t := range e.Takes {
			s, end, _ := kws.FindSpeech(t)
			ev.Takes = append(ev.Takes, float64(end-s)/kws.SampleRate)
		}
		v.Enroll = ev
	}
	v.Train = d.train
	return v
}

// Handler serves the UI and the JSON API.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.state())
	})
	mux.HandleFunc("POST /api/words/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled     *bool    `json:"enabled"`
			Sensitivity *float32 `json:"sensitivity"`
			Actions     []string `json:"actions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, err)
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		s := d.settings.Words[r.PathValue("id")]
		if s == nil {
			httpErr(w, errors.New("нет такого слова"))
			return
		}
		if req.Enabled != nil {
			s.Enabled = *req.Enabled
		}
		if req.Sensitivity != nil {
			s.Sensitivity = max(-0.3, min(0.3, *req.Sensitivity))
		}
		if req.Actions != nil {
			s.Actions = normalizeActions(req.Actions)
		}
		if err := d.saveSettings(); err != nil {
			httpErr(w, err)
			return
		}
		if err := d.rebuildDetector(); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, s)
	})
	mux.HandleFunc("DELETE /api/words/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if wordID(id) != id {
			httpErr(w, errors.New("bad id"))
			return
		}
		d.mu.Lock()
		delete(d.settings.Words, id)
		d.saveSettings()
		d.mu.Unlock()
		if err := os.RemoveAll(d.wordDir(id)); err != nil {
			httpErr(w, err)
			return
		}
		if err := d.loadWords(); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/words/{id}/retrain", func(w http.ResponseWriter, r *http.Request) {
		if err := d.startTraining(r.PathValue("id")); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/hits/{n}", func(w http.ResponseWriter, r *http.Request) {
		h := d.hit(r.PathValue("n"))
		if h == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav.Encode(h.audio, kws.SampleRate))
	})
	mux.HandleFunc("POST /api/hits/{n}/false", func(w http.ResponseWriter, r *http.Request) {
		h := d.hit(r.PathValue("n"))
		if h == nil {
			http.NotFound(w, r)
			return
		}
		p := filepath.Join(d.wordDir(h.Word), fmt.Sprintf("false-%d.wav", h.At.Unix()))
		if err := wav.WriteFile(p, h.audio, kws.SampleRate); err != nil {
			httpErr(w, err)
			return
		}
		d.mu.Lock()
		for i, x := range d.hits {
			if x == h {
				d.hits = append(d.hits[:i], d.hits[i+1:]...)
				break
			}
		}
		d.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/events", d.serveEvents)
	mux.HandleFunc("GET /api/stream", d.serveAudio)
	mux.HandleFunc("POST /api/listen", func(w http.ResponseWriter, r *http.Request) {
		go func() {
			if err := d.runAthena(context.Background()); err != nil {
				log.Printf("listen: %v", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/athena/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.athenaConfigView())
	})
	mux.HandleFunc("POST /api/athena/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AthenaURL               *string `json:"athena_url"`
			AthenaToken             string  `json:"athena_token"`
			LunaURL                 *string `json:"luna_url"`
			Channel                 *string `json:"channel"`
			RecognizeTimeoutSeconds *int    `json:"recognize_timeout_seconds"`
			TTSVoice                *string `json:"tts_voice"`
			MuteReplies             *bool   `json:"mute_replies"`
			ClearToken              bool    `json:"clear_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, err)
			return
		}
		if req.Channel != nil {
			c := strings.TrimSpace(*req.Channel)
			if c != "vqe_0" && c != "vqe_bnr" {
				httpErr(w, errors.New("канал должен быть vqe_0 или vqe_bnr"))
				return
			}
		}
		var athenaURL, lunaURL string
		var err error
		if req.AthenaURL != nil {
			if athenaURL, err = cleanHTTPURL(*req.AthenaURL); err != nil {
				httpErr(w, errors.New("athena_url: "+err.Error()))
				return
			}
		}
		if req.LunaURL != nil {
			if lunaURL, err = cleanHTTPURL(*req.LunaURL); err != nil {
				httpErr(w, errors.New("luna_url: "+err.Error()))
				return
			}
		}
		channelChanged := false
		d.mu.Lock()
		if req.AthenaURL != nil {
			d.settings.AthenaURL = athenaURL
		}
		if req.LunaURL != nil {
			d.settings.LunaURL = lunaURL
		}
		if req.Channel != nil {
			c := strings.TrimSpace(*req.Channel)
			channelChanged = c != d.settings.Channel
			d.settings.Channel = c
		}
		if req.ClearToken {
			d.settings.AthenaToken = ""
		} else if strings.TrimSpace(req.AthenaToken) != "" {
			d.settings.AthenaToken = strings.TrimSpace(req.AthenaToken)
		}
		if req.RecognizeTimeoutSeconds != nil {
			d.settings.RecognizeTimeoutSeconds = max(2, min(60, *req.RecognizeTimeoutSeconds))
		}
		if req.TTSVoice != nil {
			d.settings.TTSVoice = strings.TrimSpace(*req.TTSVoice)
		}
		if req.MuteReplies != nil {
			d.settings.MuteReplies = *req.MuteReplies
		}
		err = d.saveSettings()
		d.mu.Unlock()
		if err != nil {
			httpErr(w, err)
			return
		}
		if channelChanged {
			d.restartAudio()
		}
		writeJSON(w, d.athenaConfigView())
	})
	mux.HandleFunc("GET /api/athena/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		writeJSON(w, d.athenaHealth(ctx))
	})
	mux.HandleFunc("POST /api/athena/ask", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		if err := d.AskAthena(ctx, req.Text); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/enroll/start", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, err)
			return
		}
		if _, err := d.startEnroll(req.Name); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, d.state().Enroll)
	})
	mux.HandleFunc("POST /api/enroll/cancel", func(w http.ResponseWriter, r *http.Request) {
		d.endEnroll()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/enroll/ambient", func(w http.ResponseWriter, r *http.Request) {
		level, err := d.recordAmbient(r.Context())
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]float64{"level": level})
	})
	mux.HandleFunc("POST /api/enroll/take", func(w http.ResponseWriter, r *http.Request) {
		res, err := d.recordTake(r.Context())
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("DELETE /api/enroll/take/{i}", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(r.PathValue("i"))
		if err := d.deleteTake(i); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, d.state().Enroll)
	})
	// {i} is a take index or "last" (the latest take even if it was rejected).
	mux.HandleFunc("GET /api/enroll/take/{i}", func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(r.PathValue("i"))
		d.mu.Lock()
		var pcm []int16
		switch {
		case d.enroll == nil:
		case r.PathValue("i") == "last":
			pcm = d.enroll.Last
		case err == nil && i >= 0 && i < len(d.enroll.Takes):
			pcm = d.enroll.Takes[i]
		}
		d.mu.Unlock()
		if pcm == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav.Encode(pcm, kws.SampleRate))
	})
	mux.HandleFunc("POST /api/enroll/speech", func(w http.ResponseWriter, r *http.Request) {
		secs, _ := strconv.ParseFloat(r.URL.Query().Get("secs"), 64)
		level, err := d.recordSpeech(r.Context(), secs)
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]float64{"level": level})
	})
	mux.HandleFunc("POST /api/enroll/train", func(w http.ResponseWriter, r *http.Request) {
		if err := d.finishEnroll(); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	return withCORS(mux)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d *Daemon) hit(n string) *Hit {
	id, _ := strconv.Atoi(n)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range d.hits {
		if h.ID == id {
			return h
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, context.Canceled) {
		code = 499
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
