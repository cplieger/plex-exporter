package sessions

import (
	"strings"

	"github.com/cplieger/plex-exporter/internal/metrics"
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

// Where Plex runs one half of a video transcode.
const (
	PipelineHardware = "hardware"
	PipelineSoftware = "software"
)

// VideoPipeline reports where Plex decodes and encodes a video transcode,
// each half hardware, software or unknown; ok is false when the session is
// not transcoding video. A named hardware decoder or encoder means hardware;
// a readable transcodeHwRequested without one means software; anything else
// is unknown, because Plex sent no usable hardware fields.
func VideoPipeline(ts *plexapi.TranscodeSession) (decode, encode string, ok bool) {
	if !strings.EqualFold(strings.TrimSpace(ts.VideoDecision), metrics.ValTranscode) {
		return "", "", false
	}
	return pipelineHalf(ts.TranscodeHwDecoding, ts.TranscodeHwRequested),
		pipelineHalf(ts.TranscodeHwEncoding, ts.TranscodeHwRequested), true
}

func pipelineHalf(hw string, requested *plexapi.FlexBool) string {
	switch {
	case strings.TrimSpace(hw) != "":
		return PipelineHardware
	case requested != nil && requested.Valid():
		return PipelineSoftware
	default:
		return metrics.ValUnknown
	}
}
