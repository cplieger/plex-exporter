package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plexapi/v3"
)

// runMainEnv makes the test binary run the exporter's run() instead of the
// tests, so each start below gets a fresh process with no registry or
// marker left from another start.
const runMainEnv = "PLEX_EXPORTER_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		os.Exit(run())
	}
	os.Exit(m.Run())
}

func TestOperatorFixable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bad token 401", &plexapi.StatusError{Code: 401, Status: "401 Unauthorized", Path: "/media/providers"}, true},
		{"forbidden 403", &plexapi.StatusError{Code: 403, Status: "403 Forbidden", Path: "/"}, true},
		{"other 4xx", &plexapi.StatusError{Code: 400, Status: "400 Bad Request", Path: "/"}, true},
		{"503 is transient", &plexapi.StatusError{Code: 503, Status: "503 Service Unavailable", Path: "/"}, false},
		{"500 is transient", &plexapi.StatusError{Code: 500, Status: "500 Internal Server Error", Path: "/"}, false},
		{"429 rate limited is transient", &plexapi.StatusError{Code: 429, Status: "429 Too Many Requests", Path: "/"}, false},
		{"408 request timeout is transient", &plexapi.StatusError{Code: 408, Status: "408 Request Timeout", Path: "/"}, false},
		{"not found", fmt.Errorf("fetching providers: %w", plex.ErrNotFound), true},
		{"unknown CA", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: x509.UnknownAuthorityError{}}), true},
		{"cert verification error", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: &tls.CertificateVerificationError{Err: errors.New("x509: certificate has expired or is not yet valid")}}), true},
		{"a peer that does not speak TLS", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}), true},
		{"a fatal TLS alert", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: &net.OpError{Op: "remote error", Err: errors.New("tls: certificate required")}}), true},
		{"plain HTTP answer to https", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: http.ErrSchemeMismatch}), true},
		{"connection refused is transient", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:1/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}), false},
		{"dns failure is transient", fmt.Errorf("plex GET /: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: &net.DNSError{Err: "no such host", Name: "plex", IsNotFound: true}}), false},
		{"wrapped 401", fmt.Errorf("fetching providers: %w", &plexapi.StatusError{Code: 401, Status: "401 Unauthorized", Path: "/media/providers"}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := operatorFixable(tt.err); got != tt.want {
				t.Errorf("operatorFixable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// fakeStartPlex answers every request with status until good is set, then
// as a Plex server with no libraries.
type fakeStartPlex struct {
	status int
	good   atomic.Bool
}

func (f *fakeStartPlex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !f.good.Load() {
		w.WriteHeader(f.status)
		return
	}
	switch r.URL.Path {
	case "/media/providers":
		fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0","MediaProvider":[]}}`)
	case "/":
		fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0","platform":"Linux","platformVersion":"6.1"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// exporterProc is one exporter process started by startExporter.
type exporterProc struct {
	cmd    *exec.Cmd
	exited chan struct{}
	addr   string
	stderr strings.Builder
}

// freeAddr returns a loopback address with a port nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Setup: listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("Setup: close: %v", err)
	}
	return addr
}

// startExporter runs the exporter with env as its only PLEX_* and
// LISTEN_ADDR settings, and stops it when the test ends.
func startExporter(t *testing.T, env map[string]string) *exporterProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PLEX_") && !strings.HasPrefix(kv, "LISTEN_ADDR=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, runMainEnv+"=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	p := &exporterProc{cmd: cmd, exited: make(chan struct{}), addr: env["LISTEN_ADDR"]}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Setup: start exporter: %v", err)
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.exited
	})
	return p
}

// exitCode waits up to 20s for the process to exit and returns its code.
func (p *exporterProc) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case <-p.exited:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(20 * time.Second):
		t.Fatalf("exporter still running after 20s; stderr:\n%s", p.dump())
		return -1
	}
}

// dump kills the process and returns its stderr. exec copies stderr on its
// own goroutine until the process exits, so reading it earlier races.
func (p *exporterProc) dump() string {
	_ = p.cmd.Process.Kill()
	<-p.exited
	return p.stderr.String()
}

// stop sends SIGTERM and returns the exit code.
func (p *exporterProc) stop(t *testing.T) int {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	return p.exitCode(t)
}

// get returns the status and body of GET path, or ok false when the
// exporter does not answer.
func (p *exporterProc) get(path string) (status int, body string, ok bool) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + p.addr + path)
	if err != nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, "", false
	}
	return resp.StatusCode, string(b), true
}

// waitMetrics polls /metrics until cond holds on its body, failing the
// test if the exporter exits or 20s pass first.
func (p *exporterProc) waitMetrics(t *testing.T, what string, cond func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		select {
		case <-p.exited:
			t.Fatalf("exporter exited with code %d while waiting for %s; stderr:\n%s",
				p.cmd.ProcessState.ExitCode(), what, p.stderr.String())
		default:
		}
		if status, body, ok := p.get("/metrics"); ok && status == http.StatusOK {
			last = body
			if cond(body) {
				return body
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last /metrics:\n%s\nstderr:\n%s", what, last, p.dump())
	return ""
}

// sampleValue returns the value of the series line that starts with
// series followed by a space.
func sampleValue(body, series string) (float64, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

// plexDown reports a page that names the failed Plex refresh as its cause.
func plexDown(body string) bool {
	reach, okReach := sampleValue(body, "plex_http_reachable")
	errs, okErrs := sampleValue(body, `plex_exporter_errors_total{type="refresh"}`)
	return okReach && reach == 0 && okErrs && errs >= 1
}

func plexUp(body string) bool {
	v, ok := sampleValue(body, "plex_http_reachable")
	return ok && v == 1
}

func TestRun_starts_degraded_on_plex_refusal(t *testing.T) {
	cases := []struct {
		plex func(t *testing.T) (url string, fake *fakeStartPlex)
		name string
	}{
		{name: "rejected token", plex: func(t *testing.T) (string, *fakeStartPlex) {
			f := &fakeStartPlex{status: http.StatusUnauthorized}
			ts := httptest.NewServer(f)
			t.Cleanup(ts.Close)
			return ts.URL, f
		}},
		{name: "not found", plex: func(t *testing.T) (string, *fakeStartPlex) {
			f := &fakeStartPlex{status: http.StatusNotFound}
			ts := httptest.NewServer(f)
			t.Cleanup(ts.Close)
			return ts.URL, f
		}},
		{name: "untrusted certificate", plex: func(t *testing.T) (string, *fakeStartPlex) {
			f := &fakeStartPlex{}
			f.good.Store(true)
			ts := httptest.NewTLSServer(f)
			t.Cleanup(ts.Close)
			return ts.URL, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plexURL, fake := tc.plex(t)
			p := startExporter(t, map[string]string{
				"PLEX_URL": plexURL, "PLEX_TOKEN": "test-token", "LISTEN_ADDR": freeAddr(t),
			})
			p.waitMetrics(t, "plex_http_reachable 0 with a refresh error", plexDown)
			if status, _, ok := p.get(healthzPath); !ok || status != http.StatusOK {
				t.Errorf("%s = %d (answered %t), want 200 while serving", healthzPath, status, ok)
			}
			if fake != nil {
				fake.good.Store(true)
				p.waitMetrics(t, "plex_http_reachable 1 after Plex answers", plexUp)
			}
			if code := p.stop(t); code != 0 {
				t.Errorf("exit code after SIGTERM = %d, want 0", code)
			}
		})
	}
}

func TestRun_exits_on_settings_the_user_must_fix(t *testing.T) {
	t.Run("unset PLEX_URL", func(t *testing.T) {
		p := startExporter(t, map[string]string{"PLEX_TOKEN": "test-token", "LISTEN_ADDR": freeAddr(t)})
		if code := p.exitCode(t); code == 0 {
			t.Errorf("exit code = 0, want non-zero")
		}
	})
	t.Run("busy port", func(t *testing.T) {
		f := &fakeStartPlex{}
		f.good.Store(true)
		ts := httptest.NewServer(f)
		t.Cleanup(ts.Close)
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Setup: listen: %v", err)
		}
		t.Cleanup(func() { _ = l.Close() })
		p := startExporter(t, map[string]string{
			"PLEX_URL": ts.URL, "PLEX_TOKEN": "test-token", "LISTEN_ADDR": l.Addr().String(),
		})
		if code := p.exitCode(t); code == 0 {
			t.Errorf("exit code = 0, want non-zero")
		}
	})
}
