package main

import (
	"encoding/json"
	"fmt"
	"maps"
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
							Stacking struct {
								Mode string `json:"mode"`
							} `json:"stacking"`
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
	allowed := []string{"Largest items on $server", "Most watched titles in ${span} ${spanunit}"}
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
		Layout               *layoutNode     `json:"layout"`
		ConditionalRendering *conditionGroup `json:"conditionalRendering"`
		Title                string          `json:"title"`
		Tabs                 []layoutNode    `json:"tabs"`
		Rows                 []layoutNode    `json:"rows"`
		Items                []gridItem      `json:"items"`
		MaxColumnCount       int             `json:"maxColumnCount"`
		ColumnWidth          float64         `json:"columnWidth"`
		HideHeader           bool            `json:"hideHeader"`
	} `json:"spec"`
}

type conditionRule struct {
	Kind string `json:"kind"`
	Spec struct {
		Value    any    `json:"value"`
		Variable string `json:"variable"`
		Operator string `json:"operator"`
	} `json:"spec"`
}

type conditionGroup struct {
	Spec struct {
		Visibility string          `json:"visibility"`
		Condition  string          `json:"condition"`
		Items      []conditionRule `json:"items"`
	} `json:"spec"`
}

type gridItem = struct {
	Spec struct {
		ConditionalRendering *conditionGroup `json:"conditionalRendering"`
		Element              struct {
			Name string `json:"name"`
		} `json:"element"`
		X     int `json:"x"`
		Y     int `json:"y"`
		Width int `json:"width"`
	} `json:"spec"`
}

func child(t *testing.T, nodes []layoutNode, title string) *layoutNode {
	t.Helper()
	for i := range nodes {
		if nodes[i].Spec.Title == title && nodes[i].Spec.Layout != nil {
			return &nodes[i]
		}
	}
	t.Fatalf("Setup: no tab or row titled %q", title)
	return nil
}

func dashboardLayout(t *testing.T) layoutNode {
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
	return d.Spec.Layout
}

// row returns the layout node of the row titled title on the tab titled tab.
func row(t *testing.T, tab, title string) *layoutNode {
	t.Helper()
	root := dashboardLayout(t)
	return child(t, child(t, root.Spec.Tabs, tab).Spec.Layout.Spec.Rows, title)
}

func hasDataRule(it *gridItem) bool {
	cr := it.Spec.ConditionalRendering
	return cr != nil && slices.ContainsFunc(cr.Spec.Items, func(c conditionRule) bool {
		return c.Kind == "ConditionalRenderingData"
	})
}

// startsHiddenUntilData reports why a has-data item could show before its
// query answers, or "" when it cannot. Grafana 13.2.3 treats a has-data rule
// as undecided while its query loads and shows an item whose rules are all
// undecided, so the item also carries an always-false range rule under "any
// of". A display:none panel measures no width, so its panel pins
// maxDataPoints or its query never runs. Drop both once
// https://github.com/grafana/grafana/issues/134713 is fixed.
func startsHiddenUntilData(it *gridItem, panels map[string]dashPanel) string {
	cr := it.Spec.ConditionalRendering
	if cr.Spec.Visibility != "show" || cr.Spec.Condition != "or" {
		return fmt.Sprintf("group is %s/%s, want show/or", cr.Spec.Visibility, cr.Spec.Condition)
	}
	var rules []string
	for _, c := range cr.Spec.Items {
		rules = append(rules, fmt.Sprintf("%s=%v", c.Kind, c.Spec.Value))
	}
	slices.Sort(rules)
	if want := []string{"ConditionalRenderingData=true", "ConditionalRenderingTimeRangeSize=0s"}; !slices.Equal(rules, want) {
		return fmt.Sprintf("rules are %q, want %q", rules, want)
	}
	if n := panels[it.Spec.Element.Name].Spec.Data.Spec.QueryOptions.MaxDataPoints; n <= 0 {
		return fmt.Sprintf("maxDataPoints = %d, want a positive pin", n)
	}
	return ""
}

// attentionTiles returns the Needs attention row's items.
func attentionTiles(t *testing.T) []gridItem {
	t.Helper()
	return row(t, "Overview", "Needs attention").Spec.Layout.Spec.Items
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

// gatedGrids walks the layout and returns every auto grid holding an item
// with a has-data rule, keyed by its row's title.
func gatedGrids(t *testing.T) map[string]*layoutNode {
	t.Helper()
	out := map[string]*layoutNode{}
	var walk func(n *layoutNode, title string)
	walk = func(n *layoutNode, title string) {
		if n == nil {
			return
		}
		for i := range n.Spec.Items {
			if hasDataRule(&n.Spec.Items[i]) {
				out[title] = n
			}
		}
		for i := range n.Spec.Tabs {
			walk(n.Spec.Tabs[i].Spec.Layout, n.Spec.Tabs[i].Spec.Title)
		}
		for i := range n.Spec.Rows {
			walk(n.Spec.Rows[i].Spec.Layout, n.Spec.Rows[i].Spec.Title)
		}
	}
	root := dashboardLayout(t)
	walk(&root, "")
	if len(out) == 0 {
		t.Fatal("Setup: no item with a has-data rule; the walk no longer reads the dashboard")
	}
	return out
}

func TestDashboard_tiles_shown_only_with_data_start_hidden(t *testing.T) {
	_, panels := loadDashboard(t)
	for title, grid := range gatedGrids(t) {
		for _, it := range grid.Spec.Items {
			if !hasDataRule(&it) {
				continue
			}
			if why := startsHiddenUntilData(&it, panels); why != "" {
				t.Errorf("%s %q in row %q can show before its query answers: %s",
					it.Spec.Element.Name, panels[it.Spec.Element.Name].Spec.Title, title, why)
			}
		}
	}
}

func variableQuery(t *testing.T, name string) string {
	t.Helper()
	v, ok := dashVariables(t)[name]
	if !ok {
		t.Errorf("variable %s missing", name)
		return ""
	}
	if v.Kind != "QueryVariable" || v.Spec.Hide != "hideVariable" || v.Spec.Refresh != "onTimeRangeChanged" ||
		v.Spec.Current.Value != "0" || v.Spec.Regex != `/^\{\} ([0-9]+) /` {
		t.Errorf("variable %s is %s hide %q refresh %q saves %v regex %q, want a hidden QueryVariable refreshed on time range change, saving 0, reading the count",
			name, v.Kind, v.Spec.Hide, v.Spec.Refresh, v.Spec.Current.Value, v.Spec.Regex)
	}
	return v.dataQuery(t).Spec.Query
}

// A row still draws its header and space when every tile in it is hidden, so
// a row of tiles shown only with data shows only while a hidden variable,
// counting exactly its tiles' queries, reads above zero. Such a grid holds no
// always-shown tile, so a shown tile never trails a full line of facts.
func TestDashboard_rows_of_tiles_shown_only_with_data_show_while_one_has_data(t *testing.T) {
	_, panels := loadDashboard(t)
	root := dashboardLayout(t)
	gated := gatedGrids(t)
	for _, tab := range root.Spec.Tabs {
		for _, r := range tab.Spec.Layout.Spec.Rows {
			grid, ok := gated[r.Spec.Title]
			if !ok {
				continue
			}
			cr := r.Spec.ConditionalRendering
			if cr == nil || cr.Spec.Visibility != "show" || len(cr.Spec.Items) != 1 ||
				cr.Spec.Items[0].Kind != "ConditionalRenderingVariable" || cr.Spec.Items[0].Spec.Operator != "matches" ||
				cr.Spec.Items[0].Spec.Value != "^[1-9]" {
				t.Errorf("row %q holds tiles shown only with data but is not shown by a variable matching ^[1-9]", r.Spec.Title)
				continue
			}
			counted := variableQuery(t, cr.Spec.Items[0].Spec.Variable)
			if got := strings.Count(counted, `, "tile", "`); got != len(grid.Spec.Items) {
				t.Errorf("row %q variable counts %d queries, want one per tile, %d", r.Spec.Title, got, len(grid.Spec.Items))
			}
			for _, it := range grid.Spec.Items {
				p := panels[it.Spec.Element.Name]
				if !hasDataRule(&it) {
					t.Errorf("%q always shows but shares row %q with tiles shown only with data", p.Spec.Title, r.Spec.Title)
				}
				for _, e := range exprs(&p) {
					if !strings.Contains(counted, "count("+e+")") {
						t.Errorf("%q query is not counted by row %q's variable", p.Spec.Title, r.Spec.Title)
					}
				}
			}
		}
	}
}

// contentWidths are the widths an auto grid gets on the 1600 (menu undocked
// and docked), 1376 (menu docked) and 1024 px renders, measured in Grafana
// 13.2.3.
var contentWidths = []float64{1480, 1158, 934, 912}

// autoGridColumns returns how many tiles share a line in an auto grid width
// px wide. Grafana lays the grid out as repeat(auto-fit, minmax(min(max(100% /
// maxColumnCount - 8px, columnWidth), 100%), 1fr)) with an 8 px gap
// (AutoGridLayoutManager getTemplateColumnsTemplate), and auto-fit drops the
// empty tracks, so fewer shown tiles than columns stretch across one line.
func autoGridColumns(grid *layoutNode, width float64) int {
	track := min(max(width/float64(grid.Spec.MaxColumnCount)-8, grid.Spec.ColumnWidth), width)
	return max(int((width+8)/(track+8)), 1)
}

// The tiles for conditions that need the reader sit in a row of their own
// below the server's facts, and each shows only while its query has data.
func TestDashboard_attention_tiles_show_only_when_their_query_returns_data(t *testing.T) {
	alerts := []string{
		"plex-exporter", "Plex server", "Session poll", "Library lost items", "Libraries not read", "Library scans overdue",
		"Failed reads in ${span} ${spanunit}", "Watch history stale", "Watch figures hidden", "Libraries skipped",
		"Items without a size", "Plex update",
	}
	_, panels := loadDashboard(t)
	rows := child(t, dashboardLayout(t).Spec.Tabs, "Overview").Spec.Layout.Spec.Rows
	if len(rows) < 2 || rows[0].Spec.Title != "Server overview" || rows[1].Spec.Title != "Needs attention" {
		t.Fatalf("Overview rows start %q, want Server overview then Needs attention", rows[0].Spec.Title)
	}
	grid := rows[1].Spec.Layout
	if grid.Kind != "AutoGridLayout" {
		t.Fatalf("Needs attention layout = %q, want AutoGridLayout", grid.Kind)
	}
	var got []string
	for _, it := range grid.Spec.Items {
		title := panels[it.Spec.Element.Name].Spec.Title
		got = append(got, title)
		if !hasDataRule(&it) {
			t.Errorf("%q always shows, want it shown only while its query has data", title)
		}
	}
	if !slices.Equal(got, alerts) {
		t.Errorf("Needs attention tiles = %q, want %q", got, alerts)
	}
}

// An auto grid of N always-shown tiles has a band of widths, one track wide,
// where it lays out N-1 columns and leaves one tile alone on a line. A grid
// row on one line keeps every tile beside the others at any desktop width,
// and Grafana stacks a grid row one panel per line on a phone.
func TestDashboard_always_shown_tile_rows_fill_one_line(t *testing.T) {
	_, panels := loadDashboard(t)
	for _, c := range []struct {
		tab, row string
		want     []string
	}{
		{"Overview", "Server overview", []string{"Plex version", "Streams", "Transcodes", "Items added in ${span} ${spanunit}"}},
		{"Libraries", "Totals", []string{"Total items", "Total storage", "Total duration"}},
	} {
		grid := row(t, c.tab, c.row).Spec.Layout
		if grid.Kind != "GridLayout" {
			t.Errorf("%s layout = %q, want GridLayout", c.row, grid.Kind)
			continue
		}
		var got []string
		x := 0
		for _, it := range grid.Spec.Items {
			got = append(got, panels[it.Spec.Element.Name].Spec.Title)
			if it.Spec.Y != 0 || it.Spec.X != x {
				t.Errorf("%s %q at x %d y %d, want x %d y 0", c.row, got[len(got)-1], it.Spec.X, it.Spec.Y, x)
			}
			x = it.Spec.X + it.Spec.Width
		}
		if x != 24 {
			t.Errorf("%s tiles end at column %d, want 24", c.row, x)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s tiles = %q, want %q", c.row, got, c.want)
		}
	}
}

// Each item type that has items shows in one line beside the others, so no
// combination of types leaves a tile alone on a line at a wide width.
func TestDashboard_item_type_tiles_share_one_line(t *testing.T) {
	grid := row(t, "Libraries", "Items by type").Spec.Layout
	if grid.Kind != "AutoGridLayout" || grid.Spec.MaxColumnCount < len(grid.Spec.Items) {
		t.Fatalf("Items by type is %s with %d columns for %d tiles, want an auto grid with a column per tile",
			grid.Kind, grid.Spec.MaxColumnCount, len(grid.Spec.Items))
	}
	for _, w := range contentWidths {
		if columns := autoGridColumns(grid, w); columns < len(grid.Spec.Items) {
			t.Errorf("a %.0f px grid fits %d of the %d type tiles on a line", w, columns, len(grid.Spec.Items))
		}
	}
}

// headerEarnsItsClick reports whether a row may fold its items under a
// header: one panel does not repay the click, and the reader cannot tell
// whether more is folded there. A row of several tiles that each show only
// with data is exempt, as it lights several panels.
func headerEarnsItsClick(items []gridItem) bool {
	shown := 0
	for i := range items {
		if !hasDataRule(&items[i]) {
			shown++
		}
	}
	return shown >= 2 || (shown == 0 && len(items) >= 2)
}

func TestHeaderEarnsItsClick(t *testing.T) {
	const always = `{"spec": {}}`
	const gated = `{"spec": {"conditionalRendering": {"spec": {"items": [{"kind": "ConditionalRenderingData"}]}}}}`
	for _, c := range []struct {
		name  string
		items []string
		want  bool
	}{
		{"one panel", []string{always}, false},
		{"two panels", []string{always, always}, true},
		{"one tile shown only with data", []string{gated}, false},
		{"two tiles shown only with data", []string{gated, gated}, true},
		{"one panel beside tiles shown only with data", []string{always, gated, gated}, false},
		{"two panels beside a tile shown only with data", []string{always, always, gated}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var items []gridItem
			if err := json.Unmarshal([]byte("["+strings.Join(c.items, ",")+"]"), &items); err != nil {
				t.Fatalf("Setup: decode items: %v", err)
			}
			if got := headerEarnsItsClick(items); got != c.want {
				t.Errorf("headerEarnsItsClick(%s) = %v, want %v", c.items, got, c.want)
			}
		})
	}
}

func TestDashboard_row_headers_fold_at_least_two_panels(t *testing.T) {
	_, panels := loadDashboard(t)
	checked := 0
	for _, tab := range dashboardLayout(t).Spec.Tabs {
		for _, r := range tab.Spec.Layout.Spec.Rows {
			if r.Spec.HideHeader {
				continue
			}
			checked++
			if items := r.Spec.Layout.Spec.Items; !headerEarnsItsClick(items) {
				var titles []string
				for _, it := range items {
					titles = append(titles, panels[it.Spec.Element.Name].Spec.Title)
				}
				t.Errorf("tab %q row %q shows a header over %q, want two always-shown panels, several tiles shown only with data, or hideHeader",
					tab.Spec.Title, r.Spec.Title, titles)
			}
		}
	}
	if checked == 0 {
		t.Fatal("Setup: no row with a header; the walk no longer reads the dashboard")
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
	if got := ruleMetrics["PlexExporterSessionPollFailing"]; !slices.Equal(got, []string{"plex_session_poll_reachable", "plex_http_reachable"}) {
		t.Fatalf("Setup: PlexExporterSessionPollFailing reads %q, want plex_session_poll_reachable and plex_http_reachable", got)
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

// Without the rules loaded, Library scans overdue checks the rule's own
// comparison, so the tile and the alert share one threshold.
func TestDashboard_scan_overdue_tile_checks_the_rules_comparison(t *testing.T) {
	raw, err := os.ReadFile("alerts/promql.yaml")
	if err != nil {
		t.Fatalf("Setup: read alerts/promql.yaml: %v", err)
	}
	_, block, ok := strings.Cut(string(raw), "- alert: PlexExporterLibraryScanOverdue\n")
	_, expr, _ := strings.Cut(block, "expr: >\n")
	expr, _, _ = strings.Cut(expr, "for:")
	if !ok || strings.TrimSpace(expr) == "" {
		t.Fatal("Setup: no PlexExporterLibraryScanOverdue expr in alerts/promql.yaml")
	}
	_, panels := loadDashboard(t)
	p := panelByTitle(t, panels, "Library scans overdue")
	tile := strings.ReplaceAll(exprs(&p)[0], `{server=~"$server"}`, "")
	if rule := strings.Join(strings.Fields(expr), " "); !strings.Contains(tile, rule) {
		t.Errorf("%q = %s, want the rule's comparison %s", p.Spec.Title, tile, rule)
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
	"panel-160": {"17m"},
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
	for _, known := range []string{"Growth in ${span} ${spanunit}", "Last Plex scan", "Direct play", "Server-wide", "Nobody is streaming", "Your network", "Data source", "Annotations & Alerts", "suffix:down"} {
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
	p := panelByTitle(t, panels, "Most watched titles in ${span} ${spanunit}")
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

const spanTitle = "${span} ${spanunit}"

type dashVariable struct {
	Kind string `json:"kind"`
	Spec struct {
		Name        string `json:"name"`
		Hide        string `json:"hide"`
		Refresh     string `json:"refresh"`
		Regex       string `json:"regex"`
		SkipURLSync bool   `json:"skipUrlSync"`
		Current     struct {
			Text  any `json:"text"`
			Value any `json:"value"`
		} `json:"current"`
		// Query is a data query on a QueryVariable and a string on a
		// CustomVariable.
		Query json.RawMessage `json:"query"`
	} `json:"spec"`
}

type variableDataQuery struct {
	Group      string `json:"group"`
	Datasource struct {
		Name string `json:"name"`
	} `json:"datasource"`
	Spec struct {
		Query string `json:"query"`
	} `json:"spec"`
}

func (v *dashVariable) dataQuery(t *testing.T) variableDataQuery {
	t.Helper()
	var q variableDataQuery
	if err := json.Unmarshal(v.Spec.Query, &q); err != nil {
		t.Errorf("variable %s query %s is not a data query: %v", v.Spec.Name, v.Spec.Query, err)
	}
	return q
}

func dashVariables(t *testing.T) map[string]dashVariable {
	t.Helper()
	raw, err := os.ReadFile("grafana-dashboard.json")
	if err != nil {
		t.Fatalf("Setup: read dashboard: %v", err)
	}
	var d struct {
		Spec struct {
			Variables []dashVariable `json:"variables"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("Setup: decode dashboard: %v", err)
	}
	vars := map[string]dashVariable{}
	for _, v := range d.Spec.Variables {
		vars[v.Spec.Name] = v
	}
	return vars
}

// customText is the text Grafana gives a custom variable whose query is query,
// which titles render. The parse follows CustomVariable.transformCsvStringToOptions,
// https://github.com/grafana/scenes/blob/main/packages/scenes/src/variables/variants/CustomVariable.tsx.
// More than one option would let Grafana keep a stale choice, so it fails.
func customText(t *testing.T, query string) string {
	t.Helper()
	entries := regexp.MustCompile(`(?:\\,|[^,])+`).FindAllString(query, -1)
	if len(entries) != 1 {
		t.Errorf("custom query %q gives %d options, want 1", query, len(entries))
		return ""
	}
	if m := regexp.MustCompile(`^\s*(.+)\s:\s(.+)$`).FindStringSubmatch(entries[0]); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(entries[0])
}

// rangeWordVars are the variables a title names the range length with.
var rangeWordVars = []string{"span", "spanunit", "spanthe"}

// rangeWords is the text of each range word variable once spanread holds the
// option Grafana makes of row, or none at all (""), as on a failed first read.
func rangeWords(t *testing.T, vars map[string]dashVariable, row string) map[string]string {
	t.Helper()
	read := vars["spanread"]
	rx, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(read.Spec.Regex, "/"), "/"))
	if err != nil || rx.SubexpIndex("value") < 0 || rx.SubexpIndex("text") < 0 {
		t.Fatalf("spanread regex %q: %v, want value and text groups", read.Spec.Regex, err)
	}
	var value, text string
	if row != "" {
		m := rx.FindStringSubmatch(row)
		if m == nil {
			t.Fatalf("spanread regex %v does not match %s", rx, row)
		}
		value, text = m[rx.SubexpIndex("value")], m[rx.SubexpIndex("text")]
	}
	fill := strings.NewReplacer("${spanread:text}", text, "${spanread}", value)
	words := map[string]string{}
	for _, name := range rangeWordVars {
		var query string
		_ = json.Unmarshal(vars[name].Spec.Query, &query)
		words[name] = customText(t, fill.Replace(query))
	}
	return words
}

// A figure over the time range reads only with the range's length, so every
// panel outside the charts whose query reads $__range names it through span
// and spanunit. Every range word is a custom variable over spanread's one
// answer, so a title never pairs words from two reads, and they read "the
// range" until spanread has read a length. The lengths are spanread's query
// run at each side of every unit's threshold and at decimal roundings. The
// plex-exporter tile reads the range only to find exporters.
func TestDashboard_range_figures_name_the_range_length(t *testing.T) {
	vars := dashVariables(t)
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		if name != "spanread" && strings.Contains(string(vars[name].Spec.Query), "$__range_s") {
			t.Errorf("variable %s reads $__range_s, want spanread alone to read the range length", name)
		}
	}
	read, ok := vars["spanread"]
	if !ok {
		t.Fatal("variable spanread missing, want one read of the range length for every range word")
	}
	q := read.dataQuery(t)
	if read.Kind != "QueryVariable" || read.Spec.Hide != "hideVariable" || read.Spec.Refresh != "onTimeRangeChanged" ||
		q.Group != "prometheus" || q.Datasource.Name != "${datasource}" {
		t.Errorf("spanread is %s hide %q refresh %q on %s %q, want a hidden Prometheus QueryVariable on ${datasource}, refreshed on time range change",
			read.Kind, read.Spec.Hide, read.Spec.Refresh, q.Group, q.Datasource.Name)
	}
	if read.Spec.Current.Text != "" || read.Spec.Current.Value != "" {
		t.Errorf("spanread saves %v/%v, want it empty so a failed first read leaves the fallback",
			read.Spec.Current.Text, read.Spec.Current.Value)
	}
	if !read.Spec.SkipURLSync {
		t.Error("spanread syncs to the URL, want skipUrlSync so a shared link cannot carry a stale read")
	}
	ref := regexp.MustCompile(`\$\{?(\w+)`)
	for _, name := range rangeWordVars {
		v, ok := vars[name]
		var query string
		_ = json.Unmarshal(v.Spec.Query, &query)
		refs := ref.FindAllStringSubmatch(query, -1)
		if !ok || v.Kind != "CustomVariable" || v.Spec.Hide != "hideVariable" || len(refs) == 0 ||
			slices.ContainsFunc(refs, func(m []string) bool { return m[1] != "spanread" }) {
			t.Fatalf("variable %s is %q hide %q query %q, want a hidden CustomVariable reading only spanread",
				name, v.Kind, v.Spec.Hide, query)
		}
		if !v.Spec.SkipURLSync {
			t.Errorf("variable %s syncs to the URL, want skipUrlSync so a shared link cannot carry a stale word", name)
		}
	}
	none := rangeWords(t, vars, "")
	if want := map[string]string{"span": "the", "spanunit": "range", "spanthe": "the"}; !maps.Equal(none, want) {
		t.Errorf("range words before spanread reads a length are %v, want %v", none, want)
	}
	for _, name := range rangeWordVars {
		if c := vars[name].Spec.Current; c.Text != none[name] || c.Value != none[name] {
			t.Errorf("variable %s saves %v/%v, want %q, its text before spanread reads a length",
				name, c.Text, c.Value, none[name])
		}
	}
	for _, c := range []struct {
		rangeS     int
		span, unit string
	}{
		{1, "1", "seconds"},
		{45, "45", "seconds"},
		{119, "119", "seconds"},
		{120, "2", "minutes"},
		{150, "3", "minutes"},
		{5400, "90", "minutes"},
		{7199, "120", "minutes"},
		{7200, "2", "hours"},
		{19980, "5.6", "hours"},
		{129600, "36", "hours"},
		{172799, "48", "hours"},
		{172800, "2", "days"},
		{216000, "2.5", "days"},
		{277200, "3.2", "days"},
		{282240, "3.3", "days"},
		{2592000, "30", "days"},
	} {
		got := rangeWords(t, vars, queryResultRow(t, q.Spec.Query, c.rangeS))
		if want := map[string]string{"span": c.span, "spanunit": c.unit, "spanthe": "the " + c.span}; !maps.Equal(got, want) {
			t.Errorf("range words at a %d s range are %v, want %v", c.rangeS, got, want)
		}
	}
	_, panels := loadDashboard(t)
	figures := 0
	for name, p := range panels {
		title := p.Spec.Title
		if strings.Contains(title, "${span}") && !strings.Contains(title, spanTitle) {
			t.Errorf("%s title %q names ${span} without its unit, want %q", name, title, spanTitle)
		}
		if strings.Contains(strings.ToLower(title), "the "+spanTitle) {
			t.Errorf("%s title %q reads \"the the range\" before the length is read", name, title)
		}
		if p.Spec.VizConfig.Group == "timeseries" || name == "panel-150" {
			continue
		}
		if slices.ContainsFunc(exprs(&p), func(e string) bool { return strings.Contains(e, "$__range") }) {
			figures++
			if !strings.Contains(title, spanTitle) {
				t.Errorf("%s %q reads $__range, want the title to name %s", name, title, spanTitle)
			}
		}
	}
	if figures == 0 {
		t.Error("no range figure found; the pattern no longer reads the dashboard")
	}
}

// Each item type has its own tile, drawn only while that type has an item, so
// a server without photos shows no empty Photos box.
func TestDashboard_item_type_tiles_show_only_types_with_items(t *testing.T) {
	grid := row(t, "Libraries", "Items by type").Spec.Layout
	_, panels := loadDashboard(t)
	want := map[string]string{"Movies": "movies", "Episodes": "episodes", "Tracks": "tracks", "Photos": "photos"}
	seen := 0
	for _, it := range grid.Spec.Items {
		p := panels[it.Spec.Element.Name]
		ctype, isType := want[p.Spec.Title]
		if !isType {
			continue
		}
		seen++
		e := exprs(&p)
		if !hasDataRule(&it) || len(e) != 1 || !strings.Contains(e[0], `content_type="`+ctype+`"`) || !strings.HasSuffix(e[0], ") > 0") {
			t.Errorf("%q = %q (shown only with data %v), want one query of content_type %q ending > 0 that hides the tile when empty",
				p.Spec.Title, e, hasDataRule(&it), ctype)
		}
	}
	if seen != len(want) {
		t.Errorf("Items by type holds %d of the %d item type tiles", seen, len(want))
	}
}

// The change charts stack one band per library, each the change since the
// range start, so the stack shows every library's share of the change; a
// floor at zero would hide a library that shrank.
func TestDashboard_change_charts_stack_each_library_from_the_range_start(t *testing.T) {
	_, panels := loadDashboard(t)
	for _, c := range []struct{ title, metric string }{
		{"Storage change", "plex_library_storage_bytes"},
		{"Items change", "plex_library_items"},
	} {
		p := panelByTitle(t, panels, c.title)
		sel := c.metric + `{server=~"$server", library_id=~"$library_id"}`
		change := `sum by (server, server_id, library_id, library) (` + sel + ` - (` + sel + ` @ start()))`
		e := exprs(&p)
		if len(e) != 1 || !strings.Contains(e[0], change) {
			t.Errorf("%q = %q, want each library's change since the range start, %s", c.title, e, change)
		}
		if mode := p.Spec.VizConfig.Spec.FieldConfig.Defaults.Custom.Stacking.Mode; mode != "normal" {
			t.Errorf("%q stacks %q, want normal", c.title, mode)
		}
		if m := p.Spec.VizConfig.Spec.FieldConfig.Defaults.Min; m != nil {
			t.Errorf("%q has min %v, which hides a library that shrank", c.title, *m)
		}
	}
}
