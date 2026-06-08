package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"github.com/prometheus/client_golang/prometheus"
)

// benchServer returns a *Server pre-populated with n playing sessions.
func benchServer(n int) *Server {
	tracker := sessions.NewTracker()
	for i := range n {
		id := fmt.Sprintf("s%d", i)
		meta := plexapi.SessionMetadata{
			Type:  "movie",
			Title: "Bench Movie",
			Media: []plexapi.MediaInfo{{
				VideoResolution: "1080",
				Bitrate:         8000,
				Part:            []plexapi.MediaPart{{Decision: "copy"}},
			}},
		}
		meta.Player.Device = "Chrome"
		meta.Player.Product = "Plex Web"
		meta.Player.Local = true
		meta.Session.Location = "lan"
		meta.Session.Bandwidth = 5000
		meta.User.Title = "benchuser"

		mediaMeta := plexapi.SessionMetadata{
			Type:  "movie",
			Title: "Bench Movie",
			Media: []plexapi.MediaInfo{{VideoResolution: "1080"}},
		}

		tracker.Sessions[id] = sessions.Session{
			PlayStarted:    time.Now().Add(-time.Duration(i+1) * time.Second),
			LastUpdate:     time.Now(),
			State:          sessions.StatePlaying,
			LibName:        "Movies",
			LibID:          "1",
			LibType:        library.TypeMovie,
			TranscodeType:  metrics.ValNone,
			SubtitleAction: metrics.ValNone,
			Meta:           meta,
			MediaMeta:      mediaMeta,
		}
	}
	return &Server{
		Name:     "BenchSrv",
		ID:       "bench-id",
		Version:  "1.0",
		Platform: "Linux",
		Sessions: tracker,
		Libraries: []library.Library{
			{Name: "Movies", ID: "1", Type: library.TypeMovie, DurationTotal: 100, StorageTotal: 200, ItemsCount: 50},
		},
		ErrorCounts: make(map[string]float64, len(metrics.ErrorTypes)),
	}
}

func BenchmarkCollect(b *testing.B) {
	for _, n := range []int{0, 5, 20} {
		srv := benchServer(n)
		ch := make(chan prometheus.Metric, 256)
		b.Run(fmt.Sprintf("sessions_%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				srv.Collect(ch)
				for len(ch) > 0 {
					<-ch
				}
			}
		})
	}
}

func BenchmarkTruncLabel(b *testing.B) {
	short := "short"
	long := strings.Repeat("x", 256)
	b.Run("short", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = truncLabel(short)
		}
	})
	b.Run("long", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = truncLabel(long)
		}
	})
}
