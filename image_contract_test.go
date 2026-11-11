package main

import (
	"net"
	"os"
	"slices"
	"strings"
	"testing"
)

// dockerfileInstructions returns the Dockerfile's instructions as fields, one
// slice per line that starts an instruction.
func dockerfileInstructions(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	var out [][]string
	for line := range strings.SplitSeq(string(b), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && !strings.HasPrefix(fields[0], "#") {
			out = append(out, fields)
		}
	}
	return out
}

// TestDockerfileExposesTheDefaultListenPort pins EXPOSE to the port the
// exporter binds when LISTEN_ADDR is unset, so `docker run -P` and tools that
// read the image's ports find the listener.
func TestDockerfileExposesTheDefaultListenPort(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	_, port, err := net.SplitHostPort(listenAddr())
	if err != nil {
		t.Fatalf("default LISTEN_ADDR: %v", err)
	}
	for _, f := range dockerfileInstructions(t) {
		if f[0] == "EXPOSE" {
			if !slices.Contains(f[1:], port) {
				t.Errorf("EXPOSE lists %v, want the default listen port %s", f[1:], port)
			}
			return
		}
	}
	t.Errorf("Dockerfile has no EXPOSE, want EXPOSE %s", port)
}

// TestDockerfileStampsTheVersion pins the release pipeline's BUILD_VERSION
// build argument into main.version, which an unstamped build reports as "dev".
func TestDockerfileStampsTheVersion(t *testing.T) {
	t.Parallel()
	if version != "dev" {
		t.Errorf("unstamped version = %q, want dev", version)
	}
	var hasArg, stamps bool
	for _, f := range dockerfileInstructions(t) {
		line := strings.Join(f, " ")
		if f[0] == "ARG" && slices.Contains(f[1:], "BUILD_VERSION=dev") {
			hasArg = true
		}
		if strings.Contains(line, "go build") && strings.Contains(line, "-X main.version=${BUILD_VERSION}") {
			stamps = true
		}
	}
	if !hasArg {
		t.Error("Dockerfile declares no ARG BUILD_VERSION=dev")
	}
	if !stamps {
		t.Error("Dockerfile's go build does not set -X main.version=${BUILD_VERSION}")
	}
}
