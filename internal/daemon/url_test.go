package daemon

import "testing"

func TestCleanHTTPURL(t *testing.T) {
	ok := []string{
		"http://192.168.0.56:8000/api",
		"https://athena.example/api/",
	}
	for _, s := range ok {
		if got, err := cleanHTTPURL(s); err != nil || got == "" {
			t.Fatalf("cleanHTTPURL(%q) = %q, %v", s, got, err)
		}
	}
	bad := []string{
		"",
		"ws://192.168.0.56:8000/api/device/ws",
		"192.168.0.56:8000/api",
		"ftp://example/api",
	}
	for _, s := range bad {
		if _, err := cleanHTTPURL(s); err == nil {
			t.Fatalf("cleanHTTPURL(%q) accepted", s)
		}
	}
}
