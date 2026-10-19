package app

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr, MediaRoot, DataDir, StaticDir, MediaMarker           string
	AdminName, AdminPassword, PublicOrigin                           string
	ScanInterval, ScanSettle, SessionTTL, ToolTimeout, SpriteTimeout time.Duration
	ConvertTimeout                                                   time.Duration
	CookieSecure                                                     bool
	TrustedProxies                                                   []netip.Prefix
}

func LoadConfig() (Config, error) {
	c := Config{ListenAddr: env("LISTEN_ADDR", ":8080"), MediaRoot: env("MEDIA_ROOT", "/media"), DataDir: env("DATA_DIR", "/data"), StaticDir: os.Getenv("STATIC_DIR"), MediaMarker: os.Getenv("MEDIA_MARKER"), AdminName: os.Getenv("ADMIN_NAME"), AdminPassword: os.Getenv("ADMIN_PASSWORD"), PublicOrigin: os.Getenv("PUBLIC_ORIGIN")}
	for _, v := range []struct {
		name, fallback string
		dest           *time.Duration
		zero           bool
	}{{"SCAN_INTERVAL", "15m", &c.ScanInterval, false}, {"SCAN_SETTLE", "30s", &c.ScanSettle, true}, {"SESSION_TTL", "720h", &c.SessionTTL, false}, {"TOOL_TIMEOUT", "30s", &c.ToolTimeout, false}, {"SPRITE_TIMEOUT", "2m", &c.SpriteTimeout, false}, {"CONVERT_TIMEOUT", "2h", &c.ConvertTimeout, false}} {
		d, err := time.ParseDuration(env(v.name, v.fallback))
		if err != nil || d < 0 || (!v.zero && d == 0) {
			return c, fmt.Errorf("invalid %s", v.name)
		}
		*v.dest = d
	}
	var err error
	c.CookieSecure, err = strconv.ParseBool(env("COOKIE_SECURE", "false"))
	if err != nil {
		return c, errors.New("invalid COOKIE_SECURE")
	}
	for _, p := range []*string{&c.MediaRoot, &c.DataDir} {
		*p, err = filepath.Abs(*p)
		if err != nil {
			return c, err
		}
	}
	if c.MediaRoot == c.DataDir || within(c.MediaRoot, c.DataDir) {
		return c, errors.New("DATA_DIR must be outside MEDIA_ROOT")
	}
	if c.MediaMarker != "" && !safeRelative(c.MediaMarker) {
		return c, errors.New("MEDIA_MARKER must be a relative path inside MEDIA_ROOT")
	}
	https := false
	if c.PublicOrigin != "" {
		u, e := url.Parse(c.PublicOrigin)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, errors.New("PUBLIC_ORIGIN must be an http(s) origin")
		}
		https = u.Scheme == "https"
	}
	if c.CookieSecure != https {
		return c, errors.New("HTTPS PUBLIC_ORIGIN and COOKIE_SECURE=true must be configured together")
	}
	if raw := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); raw != "" {
		for _, value := range strings.Split(raw, ",") {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
			if err != nil {
				return c, errors.New("TRUSTED_PROXIES must be a comma-separated list of CIDRs")
			}
			c.TrustedProxies = append(c.TrustedProxies, prefix.Masked())
		}
	}
	return c, nil
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && safeRelative(rel)
}
func safeRelative(p string) bool { return p != "." && filepath.IsLocal(p) }
