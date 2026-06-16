package sessions

import (
	"strings"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plexapi"
)

// TranscodeKind classifies a transcode session by audio/video decision
// and codec changes. Return values are one of ValVideo, ValAudio,
// ValBoth, or ValNone.
func TranscodeKind(ts *plexapi.WSTranscodeSession) string {
	vDec := strings.ToLower(strings.TrimSpace(ts.VideoDecision))
	aDec := strings.ToLower(strings.TrimSpace(ts.AudioDecision))
	vSrc := strings.ToLower(strings.TrimSpace(ts.SourceVideoCodec))
	vNew := strings.ToLower(strings.TrimSpace(ts.VideoCodec))
	aSrc := strings.ToLower(strings.TrimSpace(ts.SourceAudioCodec))
	aNew := strings.ToLower(strings.TrimSpace(ts.AudioCodec))

	hasVideo := vDec == metrics.ValTranscode || (vNew != "" && vNew != vSrc)
	hasAudio := aDec == metrics.ValTranscode || (aNew != "" && aNew != aSrc)

	switch {
	case hasVideo && hasAudio:
		return metrics.ValBoth
	case hasVideo:
		return metrics.ValVideo
	case hasAudio:
		return metrics.ValAudio
	default:
		return metrics.ValNone
	}
}

// subtitleDecisionMap maps Plex wire-protocol subtitle decision strings
// to canonical Prometheus label values.
const (
	wireSubBurnIn      = "burn-in"
	wireSubCopying     = "copying"
	wireSubTranscoding = "transcoding"
)

var subtitleDecisionMap = map[string]string{
	metrics.ValBurn:      metrics.ValBurn,
	wireSubBurnIn:        metrics.ValBurn,
	metrics.ValCopy:      metrics.ValCopy,
	wireSubCopying:       metrics.ValCopy,
	metrics.ValTranscode: metrics.ValTranscode,
	wireSubTranscoding:   metrics.ValTranscode,
}

// SubtitleAction classifies a transcode session's subtitle handling.
// Return values are one of ValBurn, ValCopy, ValTranscode, ValNone, or
// "other".
func SubtitleAction(ts *plexapi.WSTranscodeSession) string {
	sd := strings.ToLower(strings.TrimSpace(ts.SubtitleDecision))
	if v, ok := subtitleDecisionMap[sd]; ok {
		return v
	}
	if sd == "" {
		ctn := strings.ToLower(strings.TrimSpace(ts.Container))
		if strings.Contains(ctn, "srt") {
			return metrics.ValCopy
		}
		if strings.ToLower(strings.TrimSpace(ts.VideoDecision)) == metrics.ValTranscode {
			return metrics.ValBurn
		}
		return metrics.ValNone
	}
	return metrics.FallbackOther
}
