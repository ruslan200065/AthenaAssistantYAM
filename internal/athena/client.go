// Package athena is a small client for the Athena Core device API.
//
// The station authenticates with a device token (X-Device-Token) and uses the
// thin-client endpoints: /device/ask and /device/tts. BaseURL is expected to
// include /api, for example http://192.168.0.56:8000/api.
package athena

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		HTTP:    &http.Client{Timeout: 45 * time.Second},
	}
}

type AskResponse struct {
	OK                     bool   `json:"ok"`
	Reply                  string `json:"reply"`
	ToolCalls              []any  `json:"tool_calls"`
	ConversationID         int64  `json:"conversation_id"`
	Listen                 bool   `json:"listen"`
	End                    bool   `json:"end"`
	ConversationState      string `json:"conversation_state"`
	FollowUpTimeoutSeconds int    `json:"follow_up_timeout_seconds"`
	LatencyMS              int    `json:"latency_ms"`
}

type apiError struct {
	Detail string `json:"detail"`
	Error  string `json:"error"`
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	if c.BaseURL == "" {
		return nil, errors.New("athena: base URL is empty")
	}
	if c.Token == "" {
		return nil, errors.New("athena: device token is empty")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Device-Token", c.Token)
	req.Header.Set("Accept", "application/json, audio/l16")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "athena-yam")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("athena: %w", err)
	}
	return resp, nil
}

func responseError(resp *http.Response, what string) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var e apiError
	_ = json.Unmarshal(b, &e)
	msg := strings.TrimSpace(e.Detail + " " + e.Error)
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("athena %s: HTTP %d: %s", what, resp.StatusCode, msg)
}

func (c *Client) Ask(ctx context.Context, text string) (*AskResponse, error) {
	body, _ := json.Marshal(map[string]any{"text": text, "speak": false})
	resp, err := c.do(ctx, http.MethodPost, "/device/ask", body, "application/json")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp, "ask")
	}
	defer resp.Body.Close()
	var out AskResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("athena ask: %w", err)
	}
	if !out.OK {
		return nil, errors.New("athena ask: backend returned ok=false")
	}
	return &out, nil
}

// TTS returns raw little-endian mono s16le PCM and its sample rate.
func (c *Client) TTS(ctx context.Context, text, voice string) ([]int16, int, error) {
	payload := map[string]any{"text": text}
	if voice != "" {
		payload["voice"] = voice
	}
	body, _ := json.Marshal(payload)
	resp, err := c.do(ctx, http.MethodPost, "/device/tts", body, "application/json")
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, responseError(resp, "tts")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, 0, err
	}
	if len(raw)%2 != 0 {
		return nil, 0, errors.New("athena tts: odd PCM byte count")
	}
	rate := 16000
	if s := resp.Header.Get("X-Sample-Rate"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 8000 && n <= 48000 {
			rate = n
		}
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return pcm, rate, nil
}

func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	resp, err := c.do(ctx, http.MethodGet, "/device/health", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp, "health")
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
