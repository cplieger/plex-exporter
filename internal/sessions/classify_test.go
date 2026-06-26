package sessions

import (
	"testing"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"pgregory.net/rapid"
)

func TestTranscodeKind(t *testing.T) {
	tests := []struct {
		name string
		ts   plexapi.WSTranscodeSession
		want string
	}{
		{
			name: "video decision transcode only",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "transcode", AudioDecision: "copy"},
			want: metrics.ValVideo,
		},
		{
			name: "audio decision transcode only",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "copy", AudioDecision: "transcode"},
			want: metrics.ValAudio,
		},
		{
			name: "both decisions transcode",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "transcode", AudioDecision: "transcode"},
			want: metrics.ValBoth,
		},
		{
			name: "direct play with no codec change",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "copy", AudioDecision: "copy"},
			want: metrics.ValNone,
		},
		{
			name: "video codec change implies a video transcode",
			ts:   plexapi.WSTranscodeSession{SourceVideoCodec: "hevc", VideoCodec: "h264"},
			want: metrics.ValVideo,
		},
		{
			name: "unchanged video codec is not a transcode",
			ts:   plexapi.WSTranscodeSession{SourceVideoCodec: "h264", VideoCodec: "h264"},
			want: metrics.ValNone,
		},
		{
			name: "audio codec change implies an audio transcode",
			ts:   plexapi.WSTranscodeSession{SourceAudioCodec: "eac3", AudioCodec: "aac"},
			want: metrics.ValAudio,
		},
		{
			name: "video codec change plus audio transcode decision is both",
			ts:   plexapi.WSTranscodeSession{SourceVideoCodec: "hevc", VideoCodec: "h264", AudioDecision: "transcode"},
			want: metrics.ValBoth,
		},
		{
			name: "decision is trimmed and lowercased before matching",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "  Transcode  "},
			want: metrics.ValVideo,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TranscodeKind(&tt.ts); got != tt.want {
				t.Errorf("TranscodeKind(%+v) = %q, want %q", tt.ts, got, tt.want)
			}
		})
	}
}

func TestSubtitleAction(t *testing.T) {
	tests := []struct {
		name string
		ts   plexapi.WSTranscodeSession
		want string
	}{
		{"burn decision", plexapi.WSTranscodeSession{SubtitleDecision: "burn"}, metrics.ValBurn},
		{"burn-in wire variant maps to burn", plexapi.WSTranscodeSession{SubtitleDecision: "burn-in"}, metrics.ValBurn},
		{"copy decision", plexapi.WSTranscodeSession{SubtitleDecision: "copy"}, metrics.ValCopy},
		{"copying wire variant maps to copy", plexapi.WSTranscodeSession{SubtitleDecision: "copying"}, metrics.ValCopy},
		{"transcode decision", plexapi.WSTranscodeSession{SubtitleDecision: "transcode"}, metrics.ValTranscode},
		{"transcoding wire variant maps to transcode", plexapi.WSTranscodeSession{SubtitleDecision: "transcoding"}, metrics.ValTranscode},
		{"empty decision with srt container copies", plexapi.WSTranscodeSession{Container: "matroska,srt"}, metrics.ValCopy},
		{"empty decision with uppercase SRT container copies", plexapi.WSTranscodeSession{Container: "SRT"}, metrics.ValCopy},
		{"empty decision with video transcode burns", plexapi.WSTranscodeSession{Container: "mkv", VideoDecision: "transcode"}, metrics.ValBurn},
		{"empty decision without srt or video transcode is none", plexapi.WSTranscodeSession{Container: "mkv", VideoDecision: "copy"}, metrics.ValNone},
		{"unknown decision falls back to other", plexapi.WSTranscodeSession{SubtitleDecision: "weird"}, metrics.FallbackOther},
		{"decision is trimmed and lowercased before matching", plexapi.WSTranscodeSession{SubtitleDecision: "  BURN  "}, metrics.ValBurn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SubtitleAction(&tt.ts); got != tt.want {
				t.Errorf("SubtitleAction(%+v) = %q, want %q", tt.ts, got, tt.want)
			}
		})
	}
}

// TestTranscodeKind_returns_known_label is a bounded-output invariant: every
// transcode_type label is a Prometheus label value bound by the canonical
// contract, so TranscodeKind must never emit anything outside the known set
// regardless of the codec/decision strings Plex sends.
func TestTranscodeKind_returns_known_label(t *testing.T) {
	known := map[string]bool{
		metrics.ValBoth:  true,
		metrics.ValVideo: true,
		metrics.ValAudio: true,
		metrics.ValNone:  true,
	}
	rapid.Check(t, func(rt *rapid.T) {
		ts := plexapi.WSTranscodeSession{
			VideoDecision:    rapid.String().Draw(rt, "videoDecision"),
			AudioDecision:    rapid.String().Draw(rt, "audioDecision"),
			SourceVideoCodec: rapid.String().Draw(rt, "sourceVideoCodec"),
			VideoCodec:       rapid.String().Draw(rt, "videoCodec"),
			SourceAudioCodec: rapid.String().Draw(rt, "sourceAudioCodec"),
			AudioCodec:       rapid.String().Draw(rt, "audioCodec"),
		}
		if got := TranscodeKind(&ts); !known[got] {
			rt.Errorf("TranscodeKind returned %q, not a canonical transcode_type label", got)
		}
	})
}

// TestSubtitleAction_returns_known_label is the same bounded-output invariant
// for the subtitle_action label: any decision/container/video-decision input
// must classify to one of the canonical label values.
func TestSubtitleAction_returns_known_label(t *testing.T) {
	known := map[string]bool{
		metrics.ValBurn:       true,
		metrics.ValCopy:       true,
		metrics.ValTranscode:  true,
		metrics.ValNone:       true,
		metrics.FallbackOther: true,
	}
	rapid.Check(t, func(rt *rapid.T) {
		ts := plexapi.WSTranscodeSession{
			SubtitleDecision: rapid.String().Draw(rt, "subtitleDecision"),
			Container:        rapid.String().Draw(rt, "container"),
			VideoDecision:    rapid.String().Draw(rt, "videoDecision"),
		}
		if got := SubtitleAction(&ts); !known[got] {
			rt.Errorf("SubtitleAction returned %q, not a canonical subtitle_action label", got)
		}
	})
}
