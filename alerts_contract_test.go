package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// releasedAlerts maps every alert name a release has shipped to "" while
// alerts/promql.yaml still holds it, or to why it was removed. Dropping a
// rule is then a decision written here, never an accident of an edit.
var releasedAlerts = map[string]string{
	"PlexExporterTargetDown":            "",
	"PlexExporterTargetAbsent":          "",
	"PlexExporterAPIUnreachable":        "",
	"PlexExporterSessionPollFailing":    "",
	"PlexExporterCollectionErrors":      "",
	"PlexExporterLibraryItemsCollapsed": "",
	"PlexExporterLibraryWalkStale":      "",
	"PlexExporterLibraryScanOverdue":    "",
	"PlexExporterSessionMapFull":        "",
	"PlexExporterRefreshIncomplete":     "",
	"PlexExporterFatalError":            "removed: a refused start keeps the exporter running, so PlexExporterAPIUnreachable reports it",
}

var alertLine = regexp.MustCompile(`(?m)^\s*- alert: (\S+)$`)

// ruleExprs returns each alert's expression in a PromQL rule file.
func ruleExprs(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Setup: read %s: %v", path, err)
	}
	out := map[string]string{}
	for _, block := range strings.Split(string(raw), "- alert: ")[1:] {
		name, rest, _ := strings.Cut(block, "\n")
		_, expr, _ := strings.Cut(rest, "expr:")
		expr, _, _ = strings.Cut(expr, "for:")
		out[strings.TrimSpace(name)] = strings.TrimPrefix(strings.TrimSpace(expr), ">")
	}
	return out
}

func TestAlerts_one_rule_file(t *testing.T) {
	files, err := filepath.Glob("alerts/*.yaml")
	if err != nil {
		t.Fatalf("Setup: glob: %v", err)
	}
	var rules []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.yaml") {
			rules = append(rules, f)
		}
	}
	if !slices.Equal(rules, []string{"alerts/promql.yaml"}) {
		t.Errorf("rule files = %v, want only alerts/promql.yaml", rules)
	}
}

func TestAlerts_released_names_are_kept_or_removed_on_purpose(t *testing.T) {
	shipped := ruleExprs(t, "alerts/promql.yaml")
	for name := range shipped {
		if why, ok := releasedAlerts[name]; !ok {
			t.Errorf("%s is not in releasedAlerts; add it as kept", name)
		} else if why != "" {
			t.Errorf("%s is listed as %q but alerts/promql.yaml still holds it", name, why)
		}
	}
	for name, why := range releasedAlerts {
		if _, ok := shipped[name]; why == "" && !ok {
			t.Errorf("%s was released and is gone from alerts/promql.yaml; list why in releasedAlerts", name)
		}
	}
	files, err := filepath.Glob("alerts/*.yaml")
	if err != nil {
		t.Fatalf("Setup: glob: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("Setup: read %s: %v", f, err)
		}
		for _, m := range alertLine.FindAllStringSubmatch(string(raw), -1) {
			if why := releasedAlerts[m[1]]; why != "" {
				t.Errorf("%s defines %s, which is %s", f, m[1], why)
			}
		}
	}
}

// publishedFamilies returns every family name the exporter's /metrics page
// can carry: the collector's descriptors and the default registry's process
// and Go collectors, which main serves beside it.
func publishedFamilies(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, d := range metrics.AllDescs {
		m := regexp.MustCompile(`fqName: "([^"]+)"`).FindStringSubmatch(d.String())
		if m == nil {
			t.Fatalf("Setup: cannot parse descriptor %s", d.String())
		}
		names = append(names, m[1])
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), collectors.NewGoCollector())
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Setup: gather: %v", err)
	}
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	return names
}

// A rule that reads a series the exporter no longer publishes goes silent
// without an error, so every metric a rule reads must be one it serves.
func TestAlerts_rules_read_only_published_series(t *testing.T) {
	published := publishedFamilies(t)
	if !slices.Contains(published, "process_start_time_seconds") {
		t.Fatalf("Setup: process_start_time_seconds missing from %v", published)
	}
	for name, expr := range ruleExprs(t, "alerts/promql.yaml") {
		for _, m := range promqlMetrics(expr) {
			if m != "up" && !slices.Contains(published, m) {
				t.Errorf("%s reads %s, which the exporter does not publish", name, m)
			}
		}
	}
}
