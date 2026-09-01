package sessions

import (
	"strings"

	"github.com/cplieger/plex-exporter/v2/internal/metrics"
	"github.com/cplieger/plexapi/v2"
)

// TranscodeKind classifies a transcode session by audio/video decision and
// codec changes. Returns ValVideo, ValAudio, ValBoth, or ValNone.
func TranscodeKind(ts *plexapi.TranscodeSession) string {
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

// subtitleDecisionMap maps Plex wire-protocol subtitle decisions to
// canonical Prometheus label values.
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
// Returns ValBurn, ValCopy, ValTranscode, ValNone, or FallbackOther.
func SubtitleAction(ts *plexapi.TranscodeSession) string {
	sd := strings.ToLower(strings.TrimSpace(ts.SubtitleDecision))
	if v, ok := subtitleDecisionMap[sd]; ok {
		return v
	}
	if sd == "" {
		// Plex always sets an explicit subtitleDecision when a subtitle
		// stream is part of the transcode, so empty means none is being
		// handled; guessing from the video decision would be wrong for a
		// video transcode with no subtitle stream. Tautulli treats an
		// empty decision as none too.
		return metrics.ValNone
	}
	return metrics.FallbackOther
}
