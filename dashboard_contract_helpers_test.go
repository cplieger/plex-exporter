package main

import (
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type promSample struct {
	labels map[string]string
	value  float64
}

// promValue is a scalar, a string or an instant vector.
type promValue struct {
	str    *string
	scalar *float64
	vector []promSample
}

// promEval evaluates the PromQL subset that a variable query over a range length
// uses, with Prometheus' semantics for each construct, and fails the test on any
// other syntax rather than guess at it.
type promEval struct {
	t    *testing.T
	toks []string
	pos  int
}

var promToken = regexp.MustCompile(`^\s*("(?:\\.|[^"\\])*"|[0-9]+(?:\.[0-9]+)?|[A-Za-z_]\w*|>=|[(),/])`)

// queryResultRow is the one row Grafana prints for query, a query_result()
// variable query, over a range of rangeS seconds: "{labels} value timestamp",
// labels in Prometheus' sorted order, as Grafana's Prometheus metric_find_query.ts
// formats it. A query giving other than one row fails.
func queryResultRow(t *testing.T, query string, rangeS int) string {
	t.Helper()
	expr, ok := strings.CutPrefix(strings.TrimSpace(query), "query_result(")
	if !ok || !strings.HasSuffix(expr, ")") {
		t.Fatalf("query %q is not a query_result() query", query)
	}
	expr = strings.ReplaceAll(strings.TrimSuffix(expr, ")"), "$__range_s", strconv.Itoa(rangeS))
	e := &promEval{t: t}
	for rest := expr; strings.TrimSpace(rest) != ""; {
		m := promToken.FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("query %q: no PromQL token this test reads at %q", query, rest)
		}
		e.toks = append(e.toks, m[1])
		rest = rest[len(m[0]):]
	}
	v := e.vector(e.or())
	if e.pos != len(e.toks) {
		t.Fatalf("query %q: unread PromQL from %q", query, e.toks[e.pos])
	}
	if len(v) != 1 {
		t.Fatalf("query %q gives %d rows at a %d s range, want 1", query, len(v), rangeS)
	}
	keys := slices.Sorted(maps.Keys(v[0].labels))
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + `="` + v[0].labels[k] + `"`
	}
	return "{" + strings.Join(pairs, ",") + "} " + strconv.FormatFloat(v[0].value, 'f', -1, 64) + " 1791585738061"
}

func (e *promEval) peek() string {
	if e.pos < len(e.toks) {
		return e.toks[e.pos]
	}
	return ""
}

func (e *promEval) expect(tok string) {
	if got := e.peek(); got != tok {
		e.t.Fatalf("PromQL: got %q at token %d, want %q", got, e.pos, tok)
	}
	e.pos++
}

func (e *promEval) next() string {
	tok := e.peek()
	e.pos++
	return tok
}

// on reads an empty on () matching clause, under which every sample matches.
func (e *promEval) on() {
	e.expect("on")
	e.expect("(")
	e.expect(")")
}

// or, and, compare and divide climb PromQL's precedence, loosest first.
func (e *promEval) or() promValue {
	l := e.and()
	for e.peek() == "or" {
		e.pos++
		e.on()
		r := e.and()
		if len(e.vector(l)) == 0 {
			l = promValue{vector: e.vector(r)}
		}
	}
	return l
}

func (e *promEval) and() promValue {
	l := e.compare()
	for e.peek() == "and" {
		e.vector(l)
		e.pos++
		e.on()
		r := e.compare()
		if len(e.vector(r)) == 0 {
			l = promValue{vector: []promSample{}}
		}
	}
	return l
}

func (e *promEval) compare() promValue {
	l := e.divide()
	if e.peek() != ">=" {
		return l
	}
	e.pos++
	limit := e.scalar(e.divide())
	var kept []promSample
	for _, s := range e.vector(l) {
		if s.value >= limit {
			kept = append(kept, s)
		}
	}
	return promValue{vector: kept}
}

func (e *promEval) divide() promValue {
	l := e.primary()
	for e.peek() == "/" {
		e.pos++
		q := e.scalar(l) / e.scalar(e.primary())
		l = promValue{scalar: &q}
	}
	return l
}

func (e *promEval) primary() promValue {
	tok := e.next()
	switch {
	case tok == "(":
		v := e.or()
		e.expect(")")
		return v
	case strings.HasPrefix(tok, `"`):
		s, err := strconv.Unquote(tok)
		if err != nil {
			e.t.Fatalf("PromQL string %s: %v", tok, err)
		}
		return promValue{str: &s}
	case tok != "" && (tok[0] >= '0' && tok[0] <= '9'):
		f, err := strconv.ParseFloat(tok, 64)
		if err != nil {
			e.t.Fatalf("PromQL number %s: %v", tok, err)
		}
		return promValue{scalar: &f}
	}
	var by []string
	if tok == "count_values" && e.peek() == "by" {
		e.pos++
		e.expect("(")
		for e.peek() != ")" {
			by = append(by, e.next())
			if e.peek() == "," {
				e.pos++
			}
		}
		e.expect(")")
	}
	e.expect("(")
	var args []promValue
	for e.peek() != ")" {
		args = append(args, e.or())
		if e.peek() == "," {
			e.pos++
		}
	}
	e.expect(")")
	return e.call(tok, by, args)
}

// call applies a function as Prometheus' promql/functions.go and aggregations do.
func (e *promEval) call(name string, by []string, args []promValue) promValue {
	arity := map[string][]int{"vector": {1}, "round": {1, 2}, "label_replace": {5}, "count_values": {2}}[name]
	if !slices.Contains(arity, len(args)) {
		e.t.Fatalf("PromQL: %s with %d arguments, not a call this test reads", name, len(args))
	}
	switch name {
	case "vector":
		return promValue{vector: []promSample{{labels: map[string]string{}, value: e.scalar(args[0])}}}
	case "round":
		to := 1.0
		if len(args) == 2 {
			to = e.scalar(args[1])
		}
		var out []promSample
		for _, s := range e.vector(args[0]) {
			out = append(out, promSample{labels: s.labels, value: math.Floor(s.value*(1/to)+0.5) / (1 / to)})
		}
		return promValue{vector: out}
	case "label_replace":
		dst, repl, src := e.str(args[1]), e.str(args[2]), e.str(args[3])
		re := regexp.MustCompile("^(?s:" + e.str(args[4]) + ")$")
		var out []promSample
		for _, s := range e.vector(args[0]) {
			labels := maps.Clone(s.labels)
			if m := re.FindStringSubmatchIndex(labels[src]); m != nil {
				if res := string(re.ExpandString(nil, repl, labels[src], m)); res != "" {
					labels[dst] = res
				} else {
					delete(labels, dst)
				}
			}
			out = append(out, promSample{labels: labels, value: s.value})
		}
		return promValue{vector: out}
	default: // count_values
		label := e.str(args[0])
		counts := map[string]*promSample{}
		var order []string
		for _, s := range e.vector(args[1]) {
			labels := map[string]string{label: strconv.FormatFloat(s.value, 'f', -1, 64)}
			for _, b := range by {
				if v, ok := s.labels[b]; ok {
					labels[b] = v
				}
			}
			var key string
			for _, k := range slices.Sorted(maps.Keys(labels)) {
				key += strconv.Quote(k) + strconv.Quote(labels[k])
			}
			if counts[key] == nil {
				counts[key] = &promSample{labels: labels}
				order = append(order, key)
			}
			counts[key].value++
		}
		var out []promSample
		for _, k := range order {
			out = append(out, *counts[k])
		}
		return promValue{vector: out}
	}
}

func (e *promEval) scalar(v promValue) float64 {
	if v.scalar == nil {
		e.t.Fatalf("PromQL: want a scalar, got %+v", v)
	}
	return *v.scalar
}

func (e *promEval) str(v promValue) string {
	if v.str == nil {
		e.t.Fatalf("PromQL: want a string, got %+v", v)
	}
	return *v.str
}

func (e *promEval) vector(v promValue) []promSample {
	if v.scalar != nil || v.str != nil {
		e.t.Fatalf("PromQL: want an instant vector, got %+v", v)
	}
	return v.vector
}
