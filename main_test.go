package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/cplieger/plex-exporter/internal/plex"
)

func TestIsFatalStartupError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bad token 401 is fatal", &plex.HTTPStatusError{Code: 401, Status: "401 Unauthorized", Path: "/media/providers"}, true},
		{"forbidden 403 is fatal", &plex.HTTPStatusError{Code: 403, Status: "403 Forbidden", Path: "/"}, true},
		{"other 4xx is fatal", &plex.HTTPStatusError{Code: 400, Status: "400 Bad Request", Path: "/"}, true},
		{"503 is transient", &plex.HTTPStatusError{Code: 503, Status: "503 Service Unavailable", Path: "/"}, false},
		{"500 is transient", &plex.HTTPStatusError{Code: 500, Status: "500 Internal Server Error", Path: "/"}, false},
		{"429 rate limited is transient", &plex.HTTPStatusError{Code: 429, Status: "429 Too Many Requests", Path: "/"}, false},
		{"408 request timeout is transient", &plex.HTTPStatusError{Code: 408, Status: "408 Request Timeout", Path: "/"}, false},
		{"not found is fatal", fmt.Errorf("fetching providers: %w", plex.ErrNotFound), true},
		{"unknown CA is fatal", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: x509.UnknownAuthorityError{}}), true},
		{"cert verification error is fatal", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: &tls.CertificateVerificationError{Err: errors.New("x509: certificate has expired or is not yet valid")}}), true},
		{"connection refused is transient", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:1/", Err: errors.New("connect: connection refused")}), false},
		{"dns failure is transient", errors.New("fetching providers: dial tcp: lookup plex: no such host"), false},
		{"wrapped 401 is fatal", fmt.Errorf("fetching providers: %w", &plex.HTTPStatusError{Code: 401, Status: "401 Unauthorized", Path: "/media/providers"}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFatalStartupError(tt.err); got != tt.want {
				t.Errorf("isFatalStartupError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("TEST_ENV_OR", "custom")
	if got := envOr("TEST_ENV_OR", "default"); got != "custom" {
		t.Errorf("envOr = %q, want custom", got)
	}
	t.Setenv("TEST_ENV_OR", "")
	if got := envOr("TEST_ENV_OR", "default"); got != "default" {
		t.Errorf("envOr = %q, want default", got)
	}
}

func TestRequireEnv(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		envVal  string
		want    string
		wantErr bool
	}{
		{name: "set", key: "TEST_REQUIRE_ENV_SET", envVal: "hello", want: "hello"},
		{name: "empty", key: "TEST_REQUIRE_ENV_EMPTY", envVal: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.envVal)
			got, err := requireEnv(tt.key)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("requireEnv(%q) = %q, want error", tt.key, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("requireEnv(%q) unexpected error: %v", tt.key, err)
			}
			if got != tt.want {
				t.Errorf("requireEnv(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}
