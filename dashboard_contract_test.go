package main

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/plex-exporter/internal/metrics"
)

type dashPanel struct {
	Spec struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Data        struct {
			Spec struct {
				Queries []struct {
					Spec struct {
						Query struct {
							Spec struct {
								Expr    string `json:"expr"`
								Instant bool   `json:"instant"`
							} `json:"spec"`
						} `json:"query"`
					} `json:"spec"`
				} `json:"queries"`
				Transformations []struct {
					Group string `json:"group"`
					Spec  struct {
						Options struct {
							ByField string `json:"byField"`
						} `json:"options"`
					} `json:"spec"`
				} `json:"transformations"`
				QueryOptions struct {
					Interval      string `json:"interval"`
					MaxDataPoints int    `json:"maxDataPoints"`
				} `json:"queryOptions"`
			} `json:"spec"`
		} `json:"data"`
		VizConfig struct {
			Group string `json:"group"`
			Spec  struct {
				FieldConfig struct {
					Defaults struct {
						Min    *float64 `json:"min"`
						Custom struct {
							CellOptions struct {
								Type string `json:"type"`
							} `json:"cellOptions"`
							DrawStyle    string `json:"drawStyle"`
							BarAlignment int    `json:"barAlignment"`
						} `json:"custom"`
						NoValue string `json:"noValue"`
					} `json:"defaults"`
					Overrides []struct {
						Matcher struct {
							ID      string          `json:"id"`
							Options json.RawMessage `json:"options"`
						} `json:"matcher"`
						Properties []struct {
							ID    string          `json:"id"`
							Value json.RawMessage `json:"value"`
						} `json:"properties"`
					} `json:"overrides"`
				} `json:"fieldConfig"`
			} `json:"spec"`
		} `json:"vizConfig"`
	} `json:"spec"`
}

func loadDashboard(t *testing.T) (layoutKind string, panels map[string]dashPanel) {
	t.Helper()
	raw, err := os.ReadFile("grafana-dashboard.json")
	if err != nil {
		t.Fatalf("Setup: read dashboard: %v", err)
	}
	var d struct {
		Spec struct {
			Layout struct {
				Kind string `json:"kind"`
			} `json:"layout"`
			Elements map[string]dashPanel `json:"elements"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("Setup: decode dashboard: %v", err)
	}
	return d.Spec.Layout.Kind, d.Spec.Elements
}

func exprs(p *dashPanel) []string {
	var out []string
	for _, q := range p.Spec.Data.Spec.Queries {
		out = append(out, q.Spec.Query.Spec.Expr)
	}
	return out
}

var (
	metricName = regexp.MustCompile(`\bplex_[a-z_]+`)
	descName   = regexp.MustCompile(`fqName: "([^"]+)"`)
	byClause   = regexp.MustCompile(`\bby \(([^)]*)\)`)
	// onRight captures the labels of an on (...) match and the grouping of
	// the aggregation that forms its right operand.
	onRight = regexp.MustCompile(`on \(([^)]*)\)(?: group_left \([^)]*\))? \(?(?:0 \* )?(?:sum|max|min|count|topk) by \(([^)]*)\)`)
)

func labelSet(s string) []string {
	var out []string
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func TestDashboard_queries_name_served_metrics(t *testing.T) {
	served := map[string]bool{}
	for _, d := range metrics.AllDescs {
		if m := descName.FindStringSubmatch(d.String()); m != nil {
			served[m[1]] = true
		}
	}
	_, panels := loadDashboard(t)
	for name, p := range panels {
		for _, e := range exprs(&p) {
			for _, m := range metricName.FindAllString(e, -1) {
				if !served[m] {
					t.Errorf("%s %q reads %s, which plex-exporter does not serve", name, p.Spec.Title, m)
				}
			}
		}
	}
}

// Item identity labels may only be grouped on by the tables built to list
// items; anywhere else they would create one series per title at query time.
func TestDashboard_item_labels_grouped_only_by_item_tables(t *testing.T) {
	allowed := []string{"Largest items on $server", "Most watched titles"}
	_, panels := loadDashboard(t)
	for name, p := range panels {
		for _, e := range exprs(&p) {
			for _, m := range byClause.FindAllStringSubmatch(e, -1) {
				labels := labelSet(m[1])
				if (slices.Contains(labels, "rating_key") || slices.Contains(labels, "title")) && !slices.Contains(allowed, p.Spec.Title) {
					t.Errorf("%s %q groups by %v", name, p.Spec.Title, labels)
				}
			}
		}
	}
}

// A vector match on labels the right operand aggregated away matches
// nothing, so every on (...) right operand keeps the labels it matches on.
func TestDashboard_on_matches_keep_their_labels(t *testing.T) {
	_, panels := loadDashboard(t)
	matches := 0
	for name, p := range panels {
		for _, e := range exprs(&p) {
			for _, m := range onRight.FindAllStringSubmatch(e, -1) {
				matches++
				kept := labelSet(m[2])
				for _, l := range labelSet(m[1]) {
					if !slices.Contains(kept, l) {
						t.Errorf("%s %q matches on %s but its right operand keeps only %v", name, p.Spec.Title, l, kept)
					}
				}
			}
		}
	}
	if matches == 0 {
		t.Error("no on (...) match found; the pattern no longer reads the dashboard")
	}
}

// A library name is free text that two servers can share, so a grouping
// that keeps the name keeps the server identity too.
func TestDashboard_library_groupings_keep_the_server(t *testing.T) {
	_, panels := loadDashboard(t)
	groupings := 0
	for name, p := range panels {
		for _, e := range exprs(&p) {
			for _, m := range byClause.FindAllStringSubmatch(e, -1) {
				labels := labelSet(m[1])
				if !slices.Contains(labels, "library") {
					continue
				}
				groupings++
				if !slices.Contains(labels, "server") || !slices.Contains(labels, "server_id") {
					t.Errorf("%s %q groups by %v, which merges equal library names across servers", name, p.Spec.Title, labels)
				}
			}
		}
	}
	if groupings == 0 {
		t.Error("no grouping by library found; the pattern no longer reads the dashboard")
	}
}

// The display name is free text two libraries on one server can share, so
// an aggregation that keeps it keeps the library identity too, and no table
// joins its queries on it.
func TestDashboard_display_name_never_replaces_the_library_identity(t *testing.T) {
	_, panels := loadDashboard(t)
	groupings := 0
	for name, p := range panels {
		for _, e := range exprs(&p) {
			for _, m := range byClause.FindAllStringSubmatch(e, -1) {
				labels := labelSet(m[1])
				if !slices.Contains(labels, "name") {
					continue
				}
				groupings++
				keyed := slices.Contains(labels, "row") || slices.Contains(labels, "server_id") && slices.Contains(labels, "library_id")
				if !keyed {
					t.Errorf("%s %q groups by %v, which merges libraries that share a name", name, p.Spec.Title, labels)
				}
			}
		}
		for _, tr := range p.Spec.Data.Spec.Transformations {
			if tr.Group == "joinByField" && tr.Spec.Options.ByField == "name" {
				t.Errorf("%s %q joins its queries on the display name", name, p.Spec.Title)
			}
		}
	}
	if groupings == 0 {
		t.Error("no grouping by name found; the pattern no longer reads the dashboard")
	}
}

// Plex numbers items per server, so an item row is keyed by its server too:
// a grouping that keeps rating_key keeps server_id, every query writes the
// hidden item key the same way (server_id, library_id and rating_key, so
// the joined queries agree), and no table joins on rating_key alone.
func TestDashboard_item_rows_keep_the_server(t *testing.T) {
	itemWrite := regexp.MustCompile(`"item", ((?:"[^"]*", )*"[^"]*")\)`)
	const itemKey = `"/", "server_id", "library_id", "rating_key"`
	_, panels := loadDashboard(t)
	keyed, built := 0, 0
	for name, p := range panels {
		for _, e := range exprs(&p) {
			for _, m := range itemWrite.FindAllStringSubmatch(e, -1) {
				built++
				if m[1] != itemKey {
					t.Errorf("%s %q writes item from %s, want %s", name, p.Spec.Title, m[1], itemKey)
				}
			}
			for _, m := range byClause.FindAllStringSubmatch(e, -1) {
				labels := labelSet(m[1])
				if slices.Contains(labels, "rating_key") && !slices.Contains(labels, "server_id") {
					t.Errorf("%s %q groups by %v, which merges items of two servers", name, p.Spec.Title, labels)
				}
				if slices.Contains(labels, "item") {
					keyed++
				}
			}
		}
		for _, tr := range p.Spec.Data.Spec.Transformations {
			if tr.Group == "joinByField" && tr.Spec.Options.ByField == "rating_key" {
				t.Errorf("%s %q joins its queries on rating_key alone", name, p.Spec.Title)
			}
		}
	}
	if keyed == 0 || built == 0 {
		t.Errorf("found %d groupings by item and %d item key writes, want both; the patterns no longer read the dashboard", keyed, built)
	}
}

// A server name is free text two servers can share, so a grouping that
// keeps it keeps server_id too.
func TestDashboard_server_groupings_keep_the_server_id(t *testing.T) {
	_, panels := loadDashboard(t)
	groupings := 0
	for name, p := range panels {
		for _, e := range exprs(&p) {
			for _, m := range byClause.FindAllStringSubmatch(e, -1) {
				labels := labelSet(m[1])
				if !slices.Contains(labels, "server") {
					continue
				}
				groupings++
				if !slices.Contains(labels, "server_id") {
					t.Errorf("%s %q groups by %v, which merges servers that report one name", name, p.Spec.Title, labels)
				}
			}
		}
	}
	if groupings == 0 {
		t.Error("no grouping by server found; the pattern no longer reads the dashboard")
	}
}

// Grafana aligns a range query's start and end down to the step, so a bar
// counting the bucket before its timestamp never draws the bucket still
// filling up, and the first point counts a bucket from before the range.
// Each bar therefore counts the bucket after its timestamp and drops a
// bucket that starts before the range.
func TestDashboard_bars_draw_the_newest_bucket_and_none_before_the_range(t *testing.T) {
	const inRange = ` and on () (vector(time()) >= $__from / 1000)`
	_, panels := loadDashboard(t)
	bars := 0
	for name, p := range panels {
		custom := p.Spec.VizConfig.Spec.FieldConfig.Defaults.Custom
		if p.Spec.VizConfig.Group != "timeseries" || custom.DrawStyle != "bars" {
			continue
		}
		bars++
		if custom.BarAlignment != 1 {
			t.Errorf("%s %q draws its bars with barAlignment %d, want 1 (the bucket after the timestamp)", name, p.Spec.Title, custom.BarAlignment)
		}
		for _, e := range exprs(&p) {
			if !strings.Contains(e, "offset -$__interval") || !strings.HasSuffix(e, inRange) {
				t.Errorf("%s %q = %s, want every bucket read with offset -$__interval and the expression ending %q", name, p.Spec.Title, e, inRange)
			}
		}
	}
	if bars == 0 {
		t.Error("no bar time series found; the pattern no longer reads the dashboard")
	}
}

// A subquery step after the range end still finds a live series through the
// 5-minute lookback, so a bucket reading forward from now counted minutes that
// had not happened yet. Each such subquery drops the steps past the range end.
func TestDashboard_bar_subqueries_count_no_step_after_the_range(t *testing.T) {
	const forward = `[$__interval:1m] offset -$__interval)`
	const clamp = ` and on () (vector(time()) <= $__to / 1000))` + forward
	_, panels := loadDashboard(t)
	subqueries := 0
	for name, p := range panels {
		if p.Spec.VizConfig.Spec.FieldConfig.Defaults.Custom.DrawStyle != "bars" {
			continue
		}
		for _, e := range exprs(&p) {
			n := strings.Count(e, forward)
			subqueries += n
			if got := strings.Count(e, clamp); got != n {
				t.Errorf("%s %q = %s: %d of %d forward subqueries end their steps at $__to, want all", name, p.Spec.Title, e, got, n)
			}
		}
	}
	if subqueries == 0 {
		t.Error("no forward subquery found in a bar chart; the pattern no longer reads the dashboard")
	}
}

// A stat reads its value off the last point of a range query, and Grafana
// aligns that point down to the step, which at the default 7 days is 30
// minutes unless the panel asks for more points; a minute keeps it current.
func TestDashboard_range_stats_show_a_current_value(t *testing.T) {
	const minutesIn7Days = 7 * 24 * 60
	_, panels := loadDashboard(t)
	ranged := 0
	for name, p := range panels {
		if p.Spec.VizConfig.Group != "stat" || p.Spec.Data.Spec.Queries[0].Spec.Query.Spec.Instant {
			continue
		}
		ranged++
		if got := p.Spec.Data.Spec.QueryOptions.MaxDataPoints; got < minutesIn7Days {
			t.Errorf("%s %q asks for %d points, want at least %d so its value is under a minute old at 7 days", name, p.Spec.Title, got, minutesIn7Days)
		}
	}
	if ranged == 0 {
		t.Error("no stat on a range query found; the pattern no longer reads the dashboard")
	}
}

// A fixed interval steps a line to the last whole interval, so a gauge
// history stepped by a day ended at midnight; only bar charts, whose bucket
// is the interval, set one.
func TestDashboard_only_bar_charts_fix_their_interval(t *testing.T) {
	_, panels := loadDashboard(t)
	for name, p := range panels {
		interval := p.Spec.Data.Spec.QueryOptions.Interval
		if interval != "" && p.Spec.VizConfig.Spec.FieldConfig.Defaults.Custom.DrawStyle != "bars" {
			t.Errorf("%s %q (%s) sets interval %q; a line follows the range's own step", name, p.Spec.Title, p.Spec.VizConfig.Group, interval)
		}
	}
}

// A gauge cell without a min scales from the smallest value in the column,
// so that row draws an empty bar; every gauge cell here starts at zero.
func TestDashboard_gauge_cells_start_at_zero(t *testing.T) {
	_, panels := loadDashboard(t)
	cells := 0
	for name, p := range panels {
		fc := &p.Spec.VizConfig.Spec.FieldConfig
		zeroByMatcher := map[string]bool{}
		for _, o := range fc.Overrides {
			for _, prop := range o.Properties {
				if prop.ID == "min" && string(prop.Value) == "0" {
					zeroByMatcher[o.Matcher.ID+string(o.Matcher.Options)] = true
				}
			}
		}
		defaultZero, minText := false, "unset"
		if fc.Defaults.Min != nil {
			defaultZero, minText = *fc.Defaults.Min == 0, strconv.FormatFloat(*fc.Defaults.Min, 'g', -1, 64)
		}
		if fc.Defaults.Custom.CellOptions.Type == "gauge" {
			cells++
			if !defaultZero {
				t.Errorf("%s %q draws every column as a gauge cell with min %s, want 0", name, p.Spec.Title, minText)
			}
		}
		for _, o := range fc.Overrides {
			for _, prop := range o.Properties {
				var cell struct {
					Type string `json:"type"`
				}
				if prop.ID != "custom.cellOptions" || json.Unmarshal(prop.Value, &cell) != nil || cell.Type != "gauge" {
					continue
				}
				cells++
				if !defaultZero && !zeroByMatcher[o.Matcher.ID+string(o.Matcher.Options)] {
					t.Errorf("%s %q draws %s as a gauge cell with min %s, want 0", name, p.Spec.Title, o.Matcher.Options, minText)
				}
			}
		}
	}
	if cells == 0 {
		t.Error("no gauge cell found; the pattern no longer reads the dashboard")
	}
}

// plex_library_items carries content_type, which the storage and duration
// families lack, so a division by it matches only after aggregating it.
func TestDashboard_item_count_divisor_is_aggregated(t *testing.T) {
	bare := regexp.MustCompile(`/\s*\(?\s*plex_library_items`)
	_, panels := loadDashboard(t)
	for name, p := range panels {
		for _, e := range exprs(&p) {
			if bare.MatchString(e) {
				t.Errorf("%s %q divides by plex_library_items without aggregating it: %s", name, p.Spec.Title, e)
			}
		}
	}
}

func TestDashboard_busy_libraries_count_each_library_once(t *testing.T) {
	_, panels := loadDashboard(t)
	p, ok := panels["panel-101"]
	if !ok {
		t.Fatal("panel-101 is missing")
	}
	e := exprs(&p)[0]
	if !strings.Contains(e, "count(count by (server, server_id, library_id) (") || !strings.Contains(e, `library_id!=""`) {
		t.Errorf("%q = %s, want a count of count by (server, server_id, library_id) over library_id!=\"\"", p.Spec.Title, e)
	}
}

func panelByTitle(t *testing.T, panels map[string]dashPanel, title string) dashPanel {
	t.Helper()
	for _, p := range panels {
		if p.Spec.Title == title {
			return p
		}
	}
	t.Fatalf("Setup: no panel titled %q", title)
	return dashPanel{}
}

// An episode series carries its show in title, so the episode leg sums by
// title alone and every row is a whole show.
func TestDashboard_most_watched_sums_episodes_per_show(t *testing.T) {
	_, panels := loadDashboard(t)
	p := panelByTitle(t, panels, "Most watched titles")
	e := exprs(&p)[0]
	const showLeg = `sum by (media_type, title) (increase(plex_play_seconds_total{server=~"$server", media_type="episode"}`
	if !strings.Contains(e, showLeg) || !strings.Contains(e, `media_type!="episode"`) {
		t.Errorf("%q = %s, want an episode leg %s… beside a media_type!=\"episode\" leg", p.Spec.Title, e, showLeg)
	}
}

func TestDashboard_panels_explain_themselves(t *testing.T) {
	kind, panels := loadDashboard(t)
	if kind != "TabsLayout" {
		t.Errorf("layout kind = %q, want TabsLayout", kind)
	}
	for name, p := range panels {
		if strings.TrimSpace(p.Spec.Description) == "" {
			t.Errorf("%s %q has no description", name, p.Spec.Title)
		}
		switch p.Spec.VizConfig.Group {
		case "stat", "gauge", "bargauge", "table":
			if strings.TrimSpace(p.Spec.VizConfig.Spec.FieldConfig.Defaults.NoValue) == "" {
				t.Errorf("%s %q has no no-value text", name, p.Spec.Title)
			}
		}
	}
}
