package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
)

// sessionsFixture is a /status/sessions answer: a hardware transcode, a
// software transcode of the same source, and a direct play.
const sessionsFixture = `{"MediaContainer":{"Metadata":[
	{"sessionKey":"hw","ratingKey":"1","type":"movie","title":"A",
	 "Player":{"product":"Plex Web","state":"playing"},"User":{"title":"u"},
	 "Media":[{"Part":[{"decision":"transcode"}]}],
	 "TranscodeSession":{"videoDecision":"transcode","sourceVideoCodec":"hevc","videoCodec":"h264",
	  "transcodeHwRequested":true,"transcodeHwDecoding":"vaapi","transcodeHwEncoding":"vaapi"}},
	{"sessionKey":"sw","ratingKey":"1","type":"movie","title":"A",
	 "Player":{"product":"Plex for Roku","state":"playing"},"User":{"title":"u"},
	 "Media":[{"Part":[{"decision":"transcode"}]}],
	 "TranscodeSession":{"videoDecision":"transcode","sourceVideoCodec":"hevc","videoCodec":"h264","transcodeHwRequested":false}},
	{"sessionKey":"dp","ratingKey":"1","type":"movie","title":"A",
	 "Player":{"product":"Infuse","state":"playing"},"User":{"title":"u"},
	 "Media":[{"Part":[{"decision":"directplay"}]}]}]}}`

func TestCollect_video_transcode_series_per_transcoding_session(t *testing.T) {
	var gone atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			if gone.Load() {
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"sessionKey":"hw","ratingKey":"1","type":"movie",
					"Player":{"state":"playing"},"Media":[{"Part":[{"decision":"directplay"}]}]}]}}`)
				return
			}
			fmt.Fprint(w, sessionsFixture)
		case "/library/metadata/1":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"type":"movie","title":"A","librarySectionID":"1"}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	srv := New(newTestClient(t, ts))
	srv.Name, srv.ID = "srv", "mid"
	srv.Libraries = []library.Library{{ID: "1", Name: "Movies", Type: library.TypeMovie}}
	srv.refreshSessions(t.Context())

	got := map[string]string{}
	for _, s := range series(t, srv, metrics.DescSessionVideoTranscode) {
		got[s.labels["session"]] = s.labels["decode"] + "/" + s.labels["encode"] + " " + s.labels["source_codec"] + ">" + s.labels["target_codec"]
	}
	want := map[string]string{"hw": "hardware/hardware hevc>h264", "sw": "software/software hevc>h264"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("video transcode series = %v, want %v", got, want)
	}

	// The hardware session drops to direct play and the software one ends.
	gone.Store(true)
	srv.refreshSessions(t.Context())
	for _, s := range series(t, srv, metrics.DescSessionVideoTranscode) {
		if s.labels["session"] == "hw" {
			t.Errorf("session hw still has a video transcode series after it stopped transcoding: %v", s.labels)
		}
		// An ended session keeps its row for a minute; it must not keep
		// adding transcode time while it waits to be pruned.
		if s.labels["session"] == "sw" && s.value != 0 {
			t.Errorf("ended session sw: video transcode value = %v, want 0", s.value)
		}
	}
}
