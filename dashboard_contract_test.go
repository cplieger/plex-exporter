package main

import (
	"encoding/json"
	"fmt"
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
					TimeFrom      string `json:"timeFrom"`
					TimeTo        string `json:"timeTo"`
					TimeShift     string `json:"timeShift"`
					TimeCompare   string `json:"timeCompare"`
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

// A stat reads its value off the last point of a range query, which Grafana
// aligns down to the step, so the default 7 days must step by at most 2m:
// gtime.RoundInterval gives 2m up to 210 s. It rounds a step down by up to
// 43 %, and for any range under about 15 years its 2m band has the tightest
// cap: past 11000·120/210 points, Prometheus refuses ranges of about 15 to 17
// days as over its 11,000-point limit (grafana/grafana#46390).
func TestDashboard_range_stats_show_a_current_value(t *testing.T) {
	const fewest, most = 7 * 24 * 3600 / 210, 11000 * 120 / 210
	_, panels := loadDashboard(t)
	ranged := 0
	for name, p := range panels {
		if p.Spec.VizConfig.Group != "stat" || p.Spec.Data.Spec.Queries[0].Spec.Query.Spec.Instant {
			continue
		}
		ranged++
		if got := p.Spec.Data.Spec.QueryOptions.MaxDataPoints; got < fewest || got > most {
			t.Errorf("%s %q asks for %d points, want %d to %d so its value is at most 2 minutes old at 7 days and no range under 15 years passes 11,000 points", name, p.Spec.Title, got, fewest, most)
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

type layoutNode struct {
	Kind string `json:"kind"`
	Spec struct {
		Layout *layoutNode  `json:"layout"`
		Title  string       `json:"title"`
		Tabs   []layoutNode `json:"tabs"`
		Rows   []layoutNode `json:"rows"`
		Items  []gridItem   `json:"items"`
	} `json:"spec"`
}

func child(t *testing.T, nodes []layoutNode, title string) *layoutNode {
	t.Helper()
	for i := range nodes {
		if nodes[i].Spec.Title == title && nodes[i].Spec.Layout != nil {
			return nodes[i].Spec.Layout
		}
	}
	t.Fatalf("Setup: no tab or row titled %q", title)
	return nil
}

type gridItem = struct {
	Spec struct {
		ConditionalRendering *struct {
			Spec struct {
				Visibility string `json:"visibility"`
				Items      []struct {
					Kind string `json:"kind"`
					Spec struct {
						Value bool `json:"value"`
					} `json:"spec"`
				} `json:"items"`
			} `json:"spec"`
		} `json:"conditionalRendering"`
		Element struct {
			Name string `json:"name"`
		} `json:"element"`
	} `json:"spec"`
}

// serverOverview returns the Overview's first grid, which holds the factual
// tiles and, after them, the tiles for conditions that need the reader.
func serverOverview(t *testing.T) *layoutNode {
	t.Helper()
	raw, err := os.ReadFile("grafana-dashboard.json")
	if err != nil {
		t.Fatalf("Setup: read dashboard: %v", err)
	}
	var d struct {
		Spec struct {
			Layout layoutNode `json:"layout"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("Setup: decode dashboard: %v", err)
	}
	overview := child(t, d.Spec.Layout.Spec.Tabs, "Overview")
	return child(t, overview.Spec.Rows, "Server overview")
}

func showsOnlyWithData(it *gridItem) bool {
	cr := it.Spec.ConditionalRendering
	return cr != nil && cr.Spec.Visibility == "show" && len(cr.Spec.Items) == 1 &&
		cr.Spec.Items[0].Kind == "ConditionalRenderingData" && cr.Spec.Items[0].Spec.Value
}

// attentionTiles returns the Server overview items drawn only while their
// query returns data.
func attentionTiles(t *testing.T) []gridItem {
	t.Helper()
	var out []gridItem
	for _, it := range serverOverview(t).Spec.Items {
		if showsOnlyWithData(&it) {
			out = append(out, it)
		}
	}
	return out
}

// Grafana queries a panel only near the viewport unless the dashboard
// preloads, and a has-data rule cannot hide a tile whose query never ran.
func TestDashboard_preloads_so_attention_tiles_below_the_fold_can_hide(t *testing.T) {
	raw, err := os.ReadFile("grafana-dashboard.json")
	if err != nil {
		t.Fatalf("Setup: read dashboard: %v", err)
	}
	var d struct {
		Spec struct {
			Preload bool `json:"preload"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("Setup: decode dashboard: %v", err)
	}
	if !d.Spec.Preload {
		t.Error("spec.preload = false, want true")
	}
}

// A GridLayoutItem carries no conditional rendering, so a tile hidden until
// its condition holds must sit in an auto grid with a has-data rule. The
// factual tiles come first so shown alerts start a line of their own.
func TestDashboard_attention_tiles_show_only_when_their_query_returns_data(t *testing.T) {
	facts := []string{"Plex version", "Streams", "Transcodes", "Plex tasks", "Oldest scan", "Items added"}
	alerts := []string{
		"plex-exporter", "Plex server", "Session poll", "Library lost items", "Libraries not read", "Failed reads",
		"Watch history stale", "Watch figures hidden", "Libraries skipped", "Items without a size", "Plex update",
	}
	_, panels := loadDashboard(t)
	grid := serverOverview(t)
	if grid.Kind != "AutoGridLayout" {
		t.Fatalf("Server overview layout = %q, want AutoGridLayout", grid.Kind)
	}
	var gotFacts, gotAlerts []string
	for _, it := range grid.Spec.Items {
		title := panels[it.Spec.Element.Name].Spec.Title
		if showsOnlyWithData(&it) {
			gotAlerts = append(gotAlerts, title)
		} else {
			if len(gotAlerts) > 0 {
				t.Errorf("%q always shows but follows an alert tile", title)
			}
			gotFacts = append(gotFacts, title)
		}
	}
	if !slices.Equal(gotFacts, facts) {
		t.Errorf("always-shown tiles = %q, want %q", gotFacts, facts)
	}
	if !slices.Equal(gotAlerts, alerts) {
		t.Errorf("tiles shown only with data = %q, want %q", gotAlerts, alerts)
	}
}

// A tile that reads an alert rule's signal names that rule, so a renamed or
// removed rule fails here instead of leaving the description pointing nowhere.
func TestDashboard_attention_tiles_name_existing_alert_rules(t *testing.T) {
	ruleName := regexp.MustCompile(`\bPlex[A-Z][A-Za-z]+\b`)
	var rules []string
	for _, f := range []string{"alerts/promql.yaml", "alerts/logql.yaml"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("Setup: read %s: %v", f, err)
		}
		for _, m := range regexp.MustCompile(`(?m)^\s*- alert: (\S+)$`).FindAllStringSubmatch(string(raw), -1) {
			rules = append(rules, m[1])
		}
	}
	_, panels := loadDashboard(t)
	for _, it := range attentionTiles(t) {
		p := panels[it.Spec.Element.Name]
		named := ruleName.FindAllString(p.Spec.Description, -1)
		if len(named) == 0 && !strings.Contains(p.Spec.Description, "No alert rule covers this") {
			t.Errorf("%q names no alert rule and does not say that none covers it", p.Spec.Title)
		}
		for _, n := range named {
			if !slices.Contains(rules, n) {
				t.Errorf("%q names %s, which is not a rule in alerts/", p.Spec.Title, n)
			}
		}
	}
}

// promqlMetrics returns the metric names a PromQL expression reads: every
// identifier left once label matchers, ranges, strings, label lists and
// function calls are stripped, minus the operators PromQL spells as words.
func promqlMetrics(expr string) []string {
	for _, strip := range []string{`"[^"]*"`, `\{[^}]*\}`, `\[[^\]]*\]`, `\b(on|by|without|ignoring|group_left|group_right)\s*\([^)]*\)`, `[A-Za-z_]\w*\s*\(`} {
		expr = regexp.MustCompile(strip).ReplaceAllString(expr, " ")
	}
	words := map[string]bool{"and": true, "or": true, "unless": true, "bool": true, "offset": true}
	var out []string
	for _, id := range regexp.MustCompile(`\b[A-Za-z_:][\w:]*\b`).FindAllString(expr, -1) {
		if !words[id] && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// A tile that names a PromQL rule reads every metric that rule reads, so the
// tile and the alert cannot be built on different signals.
func TestDashboard_attention_tiles_read_their_rules_metrics(t *testing.T) {
	raw, err := os.ReadFile("alerts/promql.yaml")
	if err != nil {
		t.Fatalf("Setup: read alerts/promql.yaml: %v", err)
	}
	ruleMetrics := map[string][]string{}
	for _, block := range strings.Split(string(raw), "- alert: ")[1:] {
		name, rest, _ := strings.Cut(block, "\n")
		_, expr, _ := strings.Cut(rest, "expr:")
		expr, _, _ = strings.Cut(expr, "for:")
		ruleMetrics[strings.TrimSpace(name)] = promqlMetrics(strings.TrimPrefix(strings.TrimSpace(expr), ">"))
	}
	if got := ruleMetrics["PlexSessionPollFailing"]; !slices.Equal(got, []string{"plex_session_poll_reachable", "plex_http_reachable"}) {
		t.Fatalf("Setup: PlexSessionPollFailing reads %q, want plex_session_poll_reachable and plex_http_reachable", got)
	}
	_, panels := loadDashboard(t)
	for _, it := range attentionTiles(t) {
		p := panels[it.Spec.Element.Name]
		tile := strings.Join(exprs(&p), "\n")
		for _, rule := range regexp.MustCompile(`\bPlex[A-Z][A-Za-z]+\b`).FindAllString(p.Spec.Description, -1) {
			for _, m := range ruleMetrics[rule] {
				if !slices.Contains(promqlMetrics(tile), m) {
					t.Errorf("%q names %s, which reads %s, but the tile's query does not", p.Spec.Title, rule, m)
				}
			}
		}
	}
}

// alertSelectors returns the rule names an expression's ALERTS selectors
// match, split by whether the selector keeps only firing alerts.
func alertSelectors(expr string) (firing, anyState []string) {
	sel := regexp.MustCompile(`ALERTS\{alertname=(~?)"([^"]+)"([^}]*)\}`)
	for _, m := range sel.FindAllStringSubmatch(expr, -1) {
		names := []string{m[2]}
		if m[1] == "~" {
			names = strings.Split(m[2], "|")
		}
		if strings.Contains(m[3], `alertstate="firing"`) {
			firing = append(firing, names...)
		} else {
			anyState = append(anyState, names...)
		}
	}
	return firing, anyState
}

// The ruler's ALERTS series is the alert's own state, so a tile that shows it
// rises and falls with the alert, whatever the evaluation phase. The tile's
// own check of the condition stands in only while the ruler has written no
// ALERTS for the rule, as on a Prometheus without alerts/promql.yaml.
func TestDashboard_attention_tiles_follow_their_rules_alert_state(t *testing.T) {
	_, panels := loadDashboard(t)
	rules := 0
	for _, it := range attentionTiles(t) {
		p := panels[it.Spec.Element.Name]
		tile := strings.Join(exprs(&p), "\n")
		firing, anyState := alertSelectors(tile)
		for _, rule := range regexp.MustCompile(`\bPlex[A-Z][A-Za-z]+\b`).FindAllString(p.Spec.Description, -1) {
			rules++
			if !slices.Contains(firing, rule) {
				t.Errorf("%q names %s, but its query reads no firing ALERTS for it", p.Spec.Title, rule)
			}
			if !slices.Contains(anyState, rule) || !strings.Contains(tile, "unless on () ") {
				t.Errorf("%q names %s, but its fallback is not dropped while ALERTS for it exist", p.Spec.Title, rule)
			}
		}
	}
	if rules == 0 {
		t.Fatal("Setup: no attention tile names an alert rule")
	}
}

// Without the rules loaded the tile checks the condition itself. A rule
// evaluated each minute fires `for` after its first true evaluation, so a
// condition held at every minute of for+2 minutes never shows before such an
// alert could fire.
func TestDashboard_attention_tiles_wait_out_their_rules_for(t *testing.T) {
	raw, err := os.ReadFile("alerts/promql.yaml")
	if err != nil {
		t.Fatalf("Setup: read alerts/promql.yaml: %v", err)
	}
	ruleFor := map[string]int{}
	for _, m := range regexp.MustCompile(`(?s)- alert: (\S+)\n.*?\n\s*for: (\d+)m\n`).FindAllStringSubmatch(string(raw), -1) {
		minutes, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("Setup: %s for: %q: %v", m[1], m[2], err)
		}
		ruleFor[m[1]] = minutes
	}
	if ruleFor["PlexExporterCollectionErrors"] != 30 {
		t.Fatalf("Setup: PlexExporterCollectionErrors for = %dm, want 30m", ruleFor["PlexExporterCollectionErrors"])
	}
	_, panels := loadDashboard(t)
	for _, it := range attentionTiles(t) {
		p := panels[it.Spec.Element.Name]
		tile := strings.Join(exprs(&p), "\n")
		for _, rule := range regexp.MustCompile(`\bPlex[A-Z][A-Za-z]+\b`).FindAllString(p.Spec.Description, -1) {
			minutes, ok := ruleFor[rule]
			if !ok {
				continue
			}
			window := fmt.Sprintf("[%dm:1m]) >= %d", minutes+2, minutes+2)
			if rule == "PlexExporterTargetAbsent" {
				window = fmt.Sprintf("[%dm:1m])", minutes+2)
			}
			if !strings.Contains(tile, window) {
				t.Errorf("%q names %s (for: %dm), but its query has no %q", p.Spec.Title, rule, minutes, window)
			}
		}
	}
}

// fixedWindows lists the literal windows a panel keeps: an alert tile's rule
// windows (for+2, the rule's own range and offset), so tile and alert agree,
// and the plex-exporter tile's week, which finds an exporter that stopped
// before a short range. Everything else reads the time picker's range.
var fixedWindows = map[string][]string{
	"panel-150": {"17m", "1w"},
	"panel-151": {"12m"},
	"panel-152": {"12m"},
	"panel-153": {"2h", "30m", "32m"},
	"panel-154": {"32m"},
	"panel-155": {"15m", "32m"},
}

var quotedString = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// literalWindows returns the fixed durations of an expression's ranges,
// subquery ranges and offsets, and its fixed @ timestamps; a subquery's step
// is a resolution, not a window. A PromQL duration may join units (1d12h),
// be bare seconds (300) and sit among spaces, so any range or offset that
// starts with a digit counts.
func literalWindows(expr string) []string {
	expr = quotedString.ReplaceAllString(expr, `""`)
	fixed := regexp.MustCompile(`\[\s*(\d[^\]:]*?)\s*(?::[^\]]*)?\]|\boffset\s+(-?)\s*(\d[\w.]*)|(@)\s*(-?\d[\w.]*)`)
	var out []string
	for _, m := range fixed.FindAllStringSubmatch(expr, -1) {
		if w := strings.Join(m[1:], ""); !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func TestDashboard_counts_over_time_follow_the_time_range(t *testing.T) {
	windowLabel := regexp.MustCompile(`\bwindow=~?"`)
	_, panels := loadDashboard(t)
	for name, p := range panels {
		if o := p.Spec.Data.Spec.QueryOptions; o.TimeFrom != "" || o.TimeTo != "" || o.TimeShift != "" || o.TimeCompare != "" {
			t.Errorf("%s %q replaces the time range (timeFrom %q, timeTo %q, timeShift %q, timeCompare %q)",
				name, p.Spec.Title, o.TimeFrom, o.TimeTo, o.TimeShift, o.TimeCompare)
		}
		var used []string
		for _, e := range exprs(&p) {
			if windowLabel.MatchString(e) {
				t.Errorf("%s %q reads a series measured over a fixed window: %s", name, p.Spec.Title, e)
			}
			for _, w := range literalWindows(e) {
				if !slices.Contains(used, w) {
					used = append(used, w)
				}
				if !slices.Contains(fixedWindows[name], w) {
					t.Errorf("%s %q measures over the fixed %s instead of the time range", name, p.Spec.Title, w)
				}
			}
		}
		for _, w := range fixedWindows[name] {
			if !slices.Contains(used, w) {
				t.Errorf("%s %q no longer uses its listed fixed window %s", name, p.Spec.Title, w)
			}
		}
	}
	for name := range fixedWindows {
		if _, ok := panels[name]; !ok {
			t.Errorf("fixedWindows lists %s, which is not a panel", name)
		}
	}
}

// visibleNames appends every tab, row, panel, link, column, series, variable
// and annotation name, every no-value and value-mapping text, and every unit
// in a decoded dashboard, field overrides included. A custom unit draws its
// text beside each value, and no built-in unit id names a period, so each unit
// counts whole. A series can take its name from a string in its query
// (label_replace), so every query string counts.
func visibleNames(v any, key string, out *[]string) {
	switch v := v.(type) {
	case map[string]any:
		if s, ok := v["value"].(string); ok {
			switch v["id"] {
			case "displayName", "noValue", "unit":
				*out = append(*out, s)
			}
		}
		if spec, ok := v["spec"].(map[string]any); ok && v["kind"] == "AnnotationQuery" {
			if s, ok := spec["name"].(string); ok {
				*out = append(*out, s)
			}
		}
		for k, child := range v {
			s, ok := child.(string)
			switch {
			case !ok:
			case k == "title" || k == "displayName" || k == "legendFormat" || k == "noValue" || k == "text" || k == "label" || k == "unit" || key == "renameByName":
				*out = append(*out, s)
			case k == "expr":
				for _, m := range quotedString.FindAllStringSubmatch(s, -1) {
					*out = append(*out, m[1])
				}
			}
			visibleNames(child, k, out)
		}
	case []any:
		for _, child := range v {
			visibleNames(child, key, out)
		}
	}
}

func TestDashboard_visible_text_names_no_fixed_period(t *testing.T) {
	period := regexp.MustCompile(`(?i)\b\d+[\s-]*(?:ms|[smhdwy]|secs?|seconds?|mins?|minutes?|hrs?|hours?|days?|wks?|weeks?|fortnights?|mos?|months?|qtrs?|quarters?|yrs?|years?)\b|` +
		`\b(?:\d+(?:ms|[smhdwy]))+\b|` +
		`\b(?:hourly|daily|weekly|fortnightly|monthly|quarterly|yearly|today|yesterday|overnight)\b|` +
		`\b(?:last|past|this|previous|prior|next|an?|one|two|three|seven|ten|twelve|thirty|ninety) ` +
		`(?:hours?|days?|weeks?|fortnights?|months?|quarters?|years?)\b`)
	raw, err := os.ReadFile("grafana-dashboard.json")
	if err != nil {
		t.Fatalf("Setup: read dashboard: %v", err)
	}
	var d any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("Setup: decode dashboard: %v", err)
	}
	// Space by when it was last watched names plex-exporter's last_watched
	// bands, which count back from now and are not a measurement period.
	ageBands := []string{"90 days to a year ago", "Within 90 days", "Over a year ago"}
	var names []string
	visibleNames(d, "", &names)
	for _, known := range []string{"Growth in this range", "Last Plex scan", "Direct play", "Storage added on $1", "Nobody is streaming", "Your network", "Data source", "Annotations & Alerts", "suffix:down"} {
		if !slices.Contains(names, known) {
			t.Fatalf("Setup: names %q miss the known name %q", names, known)
		}
	}
	for _, n := range names {
		if period.MatchString(n) && !slices.Contains(ageBands, n) {
			t.Errorf("visible text %q names a fixed period; the figure follows the time range", n)
		}
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
