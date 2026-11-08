package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

type fixedSignal bool

func (s fixedSignal) Healthy() bool { return bool(s) }

func TestRoutes_healthz(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	cases := []struct {
		name        string
		method      string
		path        string
		wantReasons []string
		serving     bool
		wantStatus  int
		wantJSON    string
	}{
		{name: "GET while serving", method: http.MethodGet, path: "/healthz", serving: true, wantStatus: http.StatusOK, wantJSON: "ok"},
		{name: "GET while not serving", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusServiceUnavailable, wantJSON: "unhealthy", wantReasons: []string{notServingReason}},
		{name: "HEAD while serving", method: http.MethodHead, path: "/healthz", serving: true, wantStatus: http.StatusOK},
		{name: "POST is refused", method: http.MethodPost, path: "/healthz", serving: true, wantStatus: http.StatusMethodNotAllowed},
		{name: "the old path is gone", method: http.MethodGet, path: "/api/health", serving: true, wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			routes(metrics, fixedSignal(tc.serving)).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.wantStatus)
			}
			if tc.wantJSON == "" {
				return
			}
			for header, want := range map[string]string{
				"Content-Type": "application/json", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
			} {
				if got := rec.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
			var body struct {
				Status  string   `json:"status"`
				Reasons []string `json:"reasons"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
			}
			if body.Status != tc.wantJSON || !slices.Equal(body.Reasons, tc.wantReasons) {
				t.Errorf("body = %q, want status %q and reasons %q", rec.Body.String(), tc.wantJSON, tc.wantReasons)
			}
		})
	}
}
