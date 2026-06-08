package main

import (
	"testing"

	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/server"
)

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

// TestNewPlexServer verifies that the run() composition root wires the
// plex client, session tracker, and initial bandwidth cursor correctly.
func TestNewPlexServer(t *testing.T) {
	client := &plex.Client{}
	srv := server.NewServer(client)
	if srv.Client != client {
		t.Error("client not set")
	}
	if srv.Sessions == nil {
		t.Error("sessions tracker not initialized")
	}
	if srv.LastBandwidthAt == 0 {
		t.Error("lastBandwidthAt should be initialized to current time")
	}
}
