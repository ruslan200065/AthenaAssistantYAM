// Package luna calls the local Luna gateway on the station.
package luna

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		HTTP:    &http.Client{Timeout: 70 * time.Second},
	}
}

type speechEvent struct {
	Type       string `json:"type"`
	Source     string `json:"source"`
	Phase      string `json:"phase"`
	RequestID  string `json:"request_id"`
	Transcript string `json:"transcript"`
	Final      bool   `json:"final"`
}

// Recognize starts stock Alice recognition in silent mode and waits for a
// final transcript. An empty transcript with no error means the timeout
// expired (Luna returns 204).
func (c *Client) Recognize(ctx context.Context, timeout time.Duration) (string, error) {
	if c.BaseURL == "" {
		return "", errors.New("luna: base URL is empty")
	}
	ms := int(timeout / time.Millisecond)
	if ms < 1000 {
		ms = 1000
	}
	if ms > 60000 {
		ms = 60000
	}
	q := url.Values{}
	q.Set("timeout_ms", strconv.Itoa(ms))
	q.Set("empty_on_timeout", "true")
	u := c.BaseURL + "/api/alice/recognize?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("luna recognize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("luna recognize: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var ev speechEvent
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		return "", fmt.Errorf("luna recognize: %w", err)
	}
	return strings.TrimSpace(ev.Transcript), nil
}

// PlayWAV queues a PCM WAV on the station's audio output. Luna requires an
// Idempotency-Key and rejects a second active job with 409.
func (c *Client) PlayWAV(ctx context.Context, wav []byte) error {
	if c.BaseURL == "" {
		return errors.New("luna: base URL is empty")
	}
	if len(wav) == 0 {
		return errors.New("luna play: empty wav")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/audio/jobs", bytes.NewReader(wav))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "audio/wav")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("athena-%d-%d", time.Now().UnixNano(), len(wav)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("luna play: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("luna play: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// SetRing paints the LED ring while an Athena turn is in progress.
func (c *Client) SetRing(ctx context.Context, colors []string) error {
	body, _ := json.Marshal(map[string]any{"colors": colors})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/led/ring", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("luna led: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// RingOff returns the ring to Luna's normal idle behaviour.
func (c *Client) RingOff(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/led/off/ring", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("luna led off: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Health checks the local Luna API and returns its HTTP status.
func (c *Client) Health(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/device", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
