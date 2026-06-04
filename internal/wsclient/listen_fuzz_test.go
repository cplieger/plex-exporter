package wsclient

import (
	"encoding/json"
	"strconv"
	"testing"

	"plex-exporter/internal/metrics"
	"plex-exporter/internal/plexapi"
	"plex-exporter/internal/sessions"
)

func FuzzWSNotificationUnmarshal(f *testing.F) {
	for _, seed := range []string{
		`{"NotificationContainer":{"type":"playing","PlaySessionStateNotification":[{"sessionKey":"1","state":"playing","viewOffset":1000}]}}`,
		`{"NotificationContainer":{"type":"transcodeSession.update","TranscodeSession":[{"key":"/transcode/sessions/abc","progress":50}]}}`,
		`{"NotificationContainer":{"type":"unknown"}}`,
		`{}`,
	} {
		f.Add([]byte(seed))
	}

	validStates := map[sessions.State]bool{
		sessions.StatePlaying: true,
		sessions.StateStopped: true,
		sessions.StatePaused:  true,
		sessions.StateOther:   true,
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var notif plexapi.WSNotification
		if err := json.Unmarshal(data, &notif); err != nil {
			return
		}
		for _, n := range notif.NotificationContainer.PlaySessionStateNotification {
			if n.RatingKey != "" {
				// RatingKey if present must be parseable as int (or handler rejects it).
				_, _ = strconv.Atoi(n.RatingKey)
			}
			state := sessions.ParseState(n.State)
			if !validStates[state] {
				t.Fatalf("ParseState(%q) returned unexpected value %q", n.State, state)
			}
		}
		for i := range notif.NotificationContainer.TranscodeSession {
			ts := &notif.NotificationContainer.TranscodeSession[i]
			kind := TranscodeKind(ts)
			if kind != metrics.ValVideo && kind != metrics.ValAudio && kind != metrics.ValBoth && kind != metrics.ValNone {
				t.Fatalf("TranscodeKind returned unexpected %q", kind)
			}
			sub := SubtitleAction(ts)
			if sub != metrics.ValBurn && sub != metrics.ValCopy && sub != metrics.ValTranscode && sub != metrics.ValNone && sub != metrics.FallbackOther {
				t.Fatalf("SubtitleAction returned unexpected %q", sub)
			}
		}
	})
}

func FuzzTranscodeKind(f *testing.F) {
	f.Add("transcode", "copy")
	f.Add("", "")
	f.Add("transcode", "transcode")
	f.Add("copy", "transcode")

	allowed := map[string]bool{
		metrics.ValVideo: true, metrics.ValAudio: true,
		metrics.ValBoth: true, metrics.ValNone: true,
	}

	f.Fuzz(func(t *testing.T, videoDecision, audioDecision string) {
		ts := &plexapi.WSTranscodeSession{
			VideoDecision: videoDecision,
			AudioDecision: audioDecision,
		}
		got := TranscodeKind(ts)
		if !allowed[got] {
			t.Fatalf("TranscodeKind(%q,%q) = %q; not in {video,audio,both,none}", videoDecision, audioDecision, got)
		}
	})
}

func FuzzSubtitleAction(f *testing.F) {
	f.Add("burn", "", "")
	f.Add("copy", "", "")
	f.Add("", "mkv", "transcode")
	f.Add("", "srt", "")
	f.Add("transcoding", "", "")

	allowed := map[string]bool{
		metrics.ValBurn: true, metrics.ValCopy: true,
		metrics.ValTranscode: true, metrics.ValNone: true,
		metrics.FallbackOther: true,
	}

	f.Fuzz(func(t *testing.T, subtitleDecision, container, videoDecision string) {
		ts := &plexapi.WSTranscodeSession{
			SubtitleDecision: subtitleDecision,
			Container:        container,
			VideoDecision:    videoDecision,
		}
		got := SubtitleAction(ts)
		if !allowed[got] {
			t.Fatalf("SubtitleAction(%q,%q,%q) = %q; not in allowed set", subtitleDecision, container, videoDecision, got)
		}
	})
}

func FuzzParseState(f *testing.F) {
	f.Add("playing")
	f.Add("stopped")
	f.Add("paused")
	f.Add("buffering")
	f.Add("")

	valid := map[sessions.State]bool{
		sessions.StatePlaying: true,
		sessions.StateStopped: true,
		sessions.StatePaused:  true,
		sessions.StateOther:   true,
	}

	f.Fuzz(func(t *testing.T, raw string) {
		got := sessions.ParseState(raw)
		if !valid[got] {
			t.Fatalf("ParseState(%q) = %q; not a known State", raw, got)
		}
	})
}
