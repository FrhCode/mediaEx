package app

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestTrustedProxyClientIP(t *testing.T) {
	s := &Server{Config: Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}}}
	for _, tc := range []struct{ name, peer, xff, want string }{
		{"untrusted spoof", "192.0.2.1:99", "198.51.100.2", "192.0.2.1"},
		{"trusted", "10.0.0.1:99", "198.51.100.2", "198.51.100.2"},
		{"chain", "10.0.0.1:99", "192.0.2.7, 198.51.100.2, 10.2.3.4", "198.51.100.2"},
		{"malformed left", "10.0.0.1:99", "bad, 198.51.100.2", "10.0.0.1"},
		{"all trusted", "10.0.0.1:99", "10.2.3.4", "10.0.0.1"},
		{"missing", "10.0.0.1:99", "", "10.0.0.1"},
		{"ipv6", "[fd00::1]:99", "2001:db8::1, fd00::2", "2001:db8::1"},
		{"mapped peer", "[::ffff:10.0.0.1]:99", "198.51.100.2", "198.51.100.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-For", tc.xff)
			r.Header.Set("X-Real-IP", "203.0.113.9")
			if got := s.clientIP(r); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	s.Config.TrustedProxies = nil
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:99"
	r.Header.Set("X-Forwarded-For", "198.51.100.2")
	if got := s.clientIP(r); got != "10.0.0.1" {
		t.Fatal(got)
	}
}

func TestProxyThrottleUsesClientIP(t *testing.T) {
	s := &Server{Config: Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}}, attempts: map[string]attempt{}}
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = "10.0.0.1:99"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	for i := 0; i < 10; i++ {
		if !s.allowLogin(s.clientIP(r)) {
			t.Fatal("blocked early")
		}
	}
	if s.allowLogin(s.clientIP(r)) {
		t.Fatal("client limit bypassed")
	}
	r.Header.Set("X-Forwarded-For", "198.51.100.2")
	if !s.allowLogin(s.clientIP(r)) {
		t.Fatal("different clients share proxy limit")
	}
}

func TestHTTPSAndProxyConfig(t *testing.T) {
	t.Setenv("MEDIA_ROOT", t.TempDir())
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("TRUSTED_PROXIES", "")
	for _, tc := range []struct {
		origin, secure string
		valid          bool
	}{
		{"", "false", true}, {"http://localhost:8080", "false", true}, {"https://media.example", "true", true},
		{"", "true", false}, {"http://localhost:8080", "true", false}, {"https://media.example", "false", false},
	} {
		t.Run(tc.origin+tc.secure, func(t *testing.T) {
			t.Setenv("PUBLIC_ORIGIN", tc.origin)
			t.Setenv("COOKIE_SECURE", tc.secure)
			_, err := LoadConfig()
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
	t.Setenv("PUBLIC_ORIGIN", "")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("TRUSTED_PROXIES", "10.0.0.1/32, fd00::/64")
	c, err := LoadConfig()
	if err != nil || len(c.TrustedProxies) != 2 {
		t.Fatal(c, err)
	}
	for _, value := range []string{"10.0.0.1", "garbage", "10.0.0.0/8,"} {
		t.Setenv("TRUSTED_PROXIES", value)
		if _, err := LoadConfig(); err == nil {
			t.Fatal(value)
		}
	}
}
