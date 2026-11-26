// Package gateway speaks the Athena Device Gateway v1 WebSocket protocol.
// It is deliberately a one-turn client: a wake hit opens a short connection,
// uploads one utterance, waits for assistant.reply and closes. The station
// sends a request only every few minutes, so connection churn costs nothing
// and keeps the code simple.
package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
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
		Firmware: "athena-yam 0.2.3",
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
	Type        string `json:"type"`
	UtteranceID string `json:"utterance_id"`
	SampleRate  int    `json:"sample_rate"`
	Format      string `json:"format"`
	Wake        bool   `json:"wake,omitempty"`
}

type audioEnd struct {
	Type        string   `json:"type"`
	UtteranceID string   `json:"utterance_id"`
	Level       *float64 `json:"level,omitempty"`
}

type frame struct {
	Type        string `json:"type"`
	UtteranceID string `json:"utterance_id"`
	Text        string `json:"text"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Reason      string `json:"reason"`
}

// Ask uploads one utterance (16 kHz mono s16le) and returns the final reply.
// It never starts native Alice and never asks the core for audio.
func (c *Client) Ask(ctx context.Context, pcm []int16) (string, error) {
	if c.BaseURL == "" {
		return "", errors.New("gateway: не задан URL Афины")
	}
	if c.Token == "" {
		return "", errors.New("gateway: не задан токен устройства")
	}
	if len(pcm) == 0 {
		return "", errors.New("gateway: нет аудио")
	}
	wsURL, err := WSURL(c.BaseURL)
	if err != nil {
		return "", err
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		return "", fmt.Errorf("gateway: connect: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)

	if err := conn.WriteJSON(hello{
		Type: "hello", DeviceID: c.DeviceID, Token: c.Token,
		Kind: "yandexstation", Firmware: c.Firmware, Hardware: c.Hardware,
		Capabilities: []string{"audio_in", "audio_out", "wake_word", "vad", "led"},
	}); err != nil {
		return "", fmt.Errorf("gateway: hello: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(12 * time.Second))
	var head frame
	if err := conn.ReadJSON(&head); err != nil {
		return "", fmt.Errorf("gateway: welcome: %w", err)
	}
	if head.Type == "error" {
		return "", fmt.Errorf("gateway: %s: %s", firstNonEmpty(head.Code, "error"), head.Message)
	}
	if head.Type != "welcome" {
		return "", fmt.Errorf("gateway: ожидался welcome, получено %q", head.Type)
	}

	id := fmt.Sprintf("u-%d", time.Now().UnixNano())
	if err := conn.WriteJSON(audioStart{
		Type: "audio.start", UtteranceID: id, SampleRate: 16000,
		Format: "pcm_s16le", Wake: true,
	}); err != nil {
		return "", fmt.Errorf("gateway: audio.start: %w", err)
	}

	// 20 ms at 16 kHz = 320 samples = 640 bytes, the documented chunk size.
	const chunk = 320
	var sum float64
	for off := 0; off < len(pcm); off += chunk {
		end := min(off+chunk, len(pcm))
		part := pcm[off:end]
		buf := make([]byte, 2*len(part))
		for i, s := range part {
			binary.LittleEndian.PutUint16(buf[2*i:], uint16(s))
			f := float64(s) / 32768
			sum += f * f
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
			return "", fmt.Errorf("gateway: audio: %w", err)
		}
	}
	level := math.Sqrt(sum / float64(len(pcm)))
	if err := conn.WriteJSON(audioEnd{Type: "audio.end", UtteranceID: id, Level: &level}); err != nil {
		return "", fmt.Errorf("gateway: audio.end: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
	for {
		var f frame
		if err := conn.ReadJSON(&f); err != nil {
			return "", fmt.Errorf("gateway: чтение ответа: %w", err)
		}
		if f.UtteranceID != "" && f.UtteranceID != id {
			continue
		}
		switch f.Type {
		case "assistant.reply":
			conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(time.Second))
			return strings.TrimSpace(f.Text), nil
		case "assistant.error", "error":
			return "", fmt.Errorf("gateway: %s: %s", firstNonEmpty(f.Code, "error"), f.Message)
		case "utterance.drop":
			return "", fmt.Errorf("gateway: запрос отклонён: %s", f.Reason)
		}
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
