package daemon

import (
	"errors"
	"net/url"
	"strings"
)

// cleanHTTPURL accepts only absolute http(s) endpoints.  The UI used to allow
// a websocket URL here by mistake; failing early is better than discovering it
// when a wake word fires.
func cleanHTTPURL(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("нужен адрес http:// или https://")
	}
	return s, nil
}

func validHTTPURL(raw string) bool {
	_, err := cleanHTTPURL(raw)
	return err == nil
}
