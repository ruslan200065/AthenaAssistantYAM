// Package gateway speaks the Athena Device Gateway v1 WebSocket protocol.
//
// A wake hit opens one session. The first turn uploads the command with
// wake=true. If the core keeps the dialogue open (reply.listen), the same
// socket is reused for follow-up turns with follow_up=true and the previous
// conversation_id. This is what makes the core's "listen after answer" work;
// a one-turn client closes the socket and silently loses every follow-up.
package gateway

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	BaseURL  string // http(s) URL including /api
	Token    string
	DeviceID string
	Firmware string
	Hardware string
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:    token,
		DeviceID: "yandexstation_2",
		Firmware: "athena-yam 0.2.6",
		Hardware: "yandexstation_2",
	}
}

// WSURL converts http://host/api to ws://host/api/device/ws.
func WSURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("gateway: неподдерживаемая схема %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("gateway: пустой адрес")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/device/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

type hello struct {
	Type         string   `json:"type"`
	DeviceID     string   `json:"device_id"`
	Token        string   `json:"token"`
	Kind         string   `json:"kind"`
	Firmware     string   `json:"firmware"`
	Hardware     string   `json:"hardware"`
	Capabilities []string `json:"capabilities"`
}

type audioStart struct {
	Type           string  `json:"type"`
	UtteranceID    string  `json:"utterance_id"`
	SampleRate     int     `json:"sample_rate"`
	Format         string  `json:"format"`
	Wake           bool    `json:"wake,omitempty"`
	FollowUp       bool    `json:"follow_up,omitempty"`
	ConversationID int64   `json:"conversation_id,omitempty"`
	WakeLevel      float64 `json:"wake_level,omitempty"`
}

type audioEnd struct {
	Type        string   `json:"type"`
	UtteranceID string   `json:"utterance_id"`
	Level       *float64 `json:"level,omitempty"`
}

type event struct {
	Type                   string          `json:"type"`
	UtteranceID            string          `json:"utterance_id"`
	Text                   string          `json:"text"`
	ConversationID         json.RawMessage `json:"conversation_id"`
	Listen                 bool            `json:"listen"`
	End                    bool            `json:"end"`
	ConversationState      string          `json:"conversation_state"`
	FollowUpTimeoutSeconds int             `json:"follow_up_timeout_seconds"`
	Reason                 string          `json:"reason"`
	Code                   string          `json:"code"`
	Message                string          `json:"message"`
}

// Reply is the part of assistant.reply that the station needs.
type Reply struct {
	Text                   string
	Listen                 bool
	End                    bool
	ConversationID         int64
	ConversationState      string
	FollowUpTimeoutSeconds int
}

// DropError means the core rejected the utterance (echo, not addressed,
// intercom, wake lost). The caller should close the microphone, not stop
// playback.
type DropError struct{ Reason string }

func (e *DropError) Error() string { return "gateway: запрос отклонён: " + e.Reason }

type Session struct {
	conn *websocket.Conn
}

// Open dials the gateway and completes hello/welcome. Keep the returned
// session for the whole conversation.
func (c *Client) Open(ctx context.Context) (*Session, error) {
	if c.BaseURL == "" {
		return nil, errors.New("gateway: не задан URL Афины")
	}
	if c.Token == "" {
		return nil, errors.New("gateway: не задан токен устройства")
	}
	wsURL, err := WSURL(c.BaseURL)
	if err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("gateway: connect: %w", err)
	}
	conn.SetReadLimit(1 << 20)

	if err := conn.WriteJSON(hello{
		Type: "hello", DeviceID: c.DeviceID, Token: c.Token,
		Kind: "yandexstation", Firmware: c.Firmware, Hardware: c.Hardware,
		Capabilities: []string{"audio_in", "audio_out", "wake_word", "vad", "led"},
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("gateway: hello: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(12 * time.Second))
	var head event
	if err := conn.ReadJSON(&head); err != nil {
		conn.Close()
		return nil, fmt.Errorf("gateway: welcome: %w", err)
	}
	if head.Type == "error" {
		conn.Close()
		return nil, fmt.Errorf("gateway: %s: %s", firstNonEmpty(head.Code, "error"), head.Message)
	}
	if head.Type != "welcome" {
		conn.Close()
		return nil, fmt.Errorf("gateway: ожидался welcome, получено %q", head.Type)
	}
	return &Session{conn: conn}, nil
}

// Turn uploads one utterance and returns assistant.reply. followUp must be
// true for every phrase after the first; conversationID continues the same
// core dialogue.
func (s *Session) Turn(ctx context.Context, pcm []int16, followUp bool, conversationID int64) (*Reply, error) {
	if s.conn == nil {
		return nil, errors.New("gateway: сессия закрыта")
	}
	if len(pcm) == 0 {
		return nil, errors.New("gateway: нет аудио")
	}
	id := fmt.Sprintf("u-%d", time.Now().UnixNano())

	start := audioStart{
		Type: "audio.start", UtteranceID: id, SampleRate: 16000,
		Format: "pcm_s16le",
	}
	if followUp {
		start.FollowUp = true
		start.ConversationID = conversationID
	} else {
		start.Wake = true
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := s.conn.WriteJSON(start); err != nil {
		return nil, fmt.Errorf("gateway: audio.start: %w", err)
	}

	// 20 ms at 16 kHz = 320 samples = 640 bytes, the documented chunk size.
	const chunk = 320
	var sum float64
	for off := 0; off < len(pcm); off += chunk {
		end := min(off+chunk, len(pcm))
		part := pcm[off:end]
		buf := make([]byte, 2*len(part))
		for i, v := range part {
			binary.LittleEndian.PutUint16(buf[2*i:], uint16(v))
			f := float64(v) / 32768
			sum += f * f
		}
		if err := s.conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
			return nil, fmt.Errorf("gateway: audio: %w", err)
		}
	}
	level := math.Sqrt(sum / float64(len(pcm)))
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := s.conn.WriteJSON(audioEnd{Type: "audio.end", UtteranceID: id, Level: &level}); err != nil {
		return nil, fmt.Errorf("gateway: audio.end: %w", err)
	}

	_ = s.conn.SetReadDeadline(time.Now().Add(75 * time.Second))
	for {
		var ev event
		if err := s.conn.ReadJSON(&ev); err != nil {
			return nil, fmt.Errorf("gateway: чтение ответа: %w", err)
		}
		if ev.UtteranceID != "" && ev.UtteranceID != id {
			continue
		}
		switch ev.Type {
		case "assistant.reply":
			return &Reply{
				Text:                   strings.TrimSpace(ev.Text),
				Listen:                 ev.Listen,
				End:                    ev.End,
				ConversationID:         parseID(ev.ConversationID),
				ConversationState:      ev.ConversationState,
				FollowUpTimeoutSeconds: ev.FollowUpTimeoutSeconds,
			}, nil
		case "conversation.end":
			// The core closes the dialogue before/without a reply.
			return &Reply{
				End:            true,
				ConversationID: parseID(ev.ConversationID),
			}, nil
		case "utterance.drop":
			return nil, &DropError{Reason: firstNonEmpty(ev.Reason, "unknown")}
		case "assistant.error", "error":
			return nil, fmt.Errorf("gateway: %s: %s", firstNonEmpty(ev.Code, "error"), ev.Message)
		case "listen.ack", "asr.partial", "asr.final", "assistant.thinking",
			"audio.url", "audio.done", "assistant.pause", "audio.duck":
			// Not needed: the station synthesizes and plays its own TTS.
		}
	}
}

// PlaybackStopped tells the core that our TTS finished. The core then ends
// the echo window and restores ducked music.
func (s *Session) PlaybackStopped(ctx context.Context) error {
	if s.conn == nil {
		return nil
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteJSON(map[string]any{
		"type": "playback.stopped", "reason": "finished",
	})
}

// EndConversation frees the wake owner and closes the dialogue on the core.
func (s *Session) EndConversation(ctx context.Context, conversationID int64) error {
	if s.conn == nil {
		return nil
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteJSON(map[string]any{
		"type": "conversation.end", "conversation_id": conversationID,
	})
}

func (s *Session) Close() {
	if s.conn == nil {
		return
	}
	_ = s.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	_ = s.conn.Close()
	s.conn = nil
}

// Ask is the old one-turn helper, kept for callers that do not need
// follow-ups. It opens, sends, closes.
func (c *Client) Ask(ctx context.Context, pcm []int16) (string, error) {
	s, err := c.Open(ctx)
	if err != nil {
		return "", err
	}
	defer s.Close()
	reply, err := s.Turn(ctx, pcm, false, 0)
	if err != nil {
		return "", err
	}
	return reply.Text, nil
}

func parseID(raw json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
