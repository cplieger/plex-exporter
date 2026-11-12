package main

import (
	"os"
	"regexp"
	"testing"
)

// The documented Kubernetes probes must ask the route the exporter serves,
// or a copied manifest restarts a healthy pod.
func TestKubernetesDoc_probes_ask_the_health_route(t *testing.T) {
	raw, err := os.ReadFile("docs/kubernetes.md")
	if err != nil {
		t.Fatalf("Setup: read docs/kubernetes.md: %v", err)
	}
	paths := regexp.MustCompile(`(?m)^\s*path: (\S+)$`).FindAllStringSubmatch(string(raw), -1)
	if len(paths) < 2 {
		t.Fatalf("found %d probe paths in docs/kubernetes.md, want the liveness and readiness probes", len(paths))
	}
	for _, p := range paths {
		if p[1] != healthzPath {
			t.Errorf("docs/kubernetes.md probes %s, want %s", p[1], healthzPath)
		}
	}
}
