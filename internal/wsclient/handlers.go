package wsclient

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"golang.org/x/sync/errgroup"
)

// HandlePlaying applies a "playing"-type websocket notification to the
// session tracker. Stopped notifications are applied without a
// /status/sessions fetch (they don't need session metadata and must
// not be dropped when the sessions API is temporarily down); all
// other states require a metadata fetch to populate labels.
func (l *Listener) HandlePlaying(ctx context.Context, notif plexapi.WSNotification) {
	// Process stop notifications first — they don't need session metadata
	// and must not be dropped when the sessions API is temporarily down.
	var needsMeta []plexapi.WSPlayNotification
	for _, n := range notif.NotificationContainer.PlaySessionStateNotification {
		state := sessions.ParseState(n.State)
		if state == sessions.StateStopped {
			l.Sessions.Update(n.SessionKey, state, nil, nil)
			continue
		}
		needsMeta = append(needsMeta, n)
	}

	if len(needsMeta) == 0 {
		return
	}

	var sessResp plexapi.MC[plexapi.MetadataListResponse]
	if err := l.Client.Get(ctx, "/status/sessions", &sessResp); err != nil {
		slog.Warn("failed to fetch sessions", "error", err)
		l.RecordError("sessions_fetch")
		return
	}

	sessionsByKey := make(map[string]*plexapi.SessionMetadata, len(sessResp.MediaContainer.Metadata))
	for i := range sessResp.MediaContainer.Metadata {
		m := &sessResp.MediaContainer.Metadata[i]
		sessionsByKey[m.SessionKey] = m
	}

	// Validate notifications before batching metadata fetches.
	type validNotif struct {
		sess  *plexapi.SessionMetadata
		state sessions.State
		n     plexapi.WSPlayNotification
	}
	valid := make([]validNotif, 0, len(needsMeta))
	for _, n := range needsMeta {
		state := sessions.ParseState(n.State)
		sess, ok := sessionsByKey[n.SessionKey]
		if !ok {
			slog.Debug("session not found", "key", n.SessionKey)
			continue
		}
		if _, err := strconv.Atoi(n.RatingKey); err != nil {
			slog.Warn("invalid rating key", "key", n.RatingKey)
			l.RecordError("invalid_rating_key")
			continue
		}
		valid = append(valid, validNotif{n: n, state: state, sess: sess})
	}

	if len(valid) == 0 {
		return
	}

	// Batch fetch metadata concurrently with bounded parallelism.
	var mu sync.Mutex
	results := make(map[int]*plexapi.SessionMetadata, len(valid))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(min(4, len(valid)))
	for i, v := range valid {
		g.Go(func() error {
			var metaResp plexapi.MC[plexapi.MetadataListResponse]
			if err := l.Client.Get(gctx, "/library/metadata/"+v.n.RatingKey, &metaResp); err != nil {
				slog.Warn("failed to fetch metadata", "key", v.n.RatingKey, "error", err)
				l.RecordError("metadata_fetch")
				return nil
			}
			if len(metaResp.MediaContainer.Metadata) == 0 {
				slog.Debug("empty metadata response", "key", v.n.RatingKey)
				return nil
			}
			mu.Lock()
			results[i] = &metaResp.MediaContainer.Metadata[0]
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		slog.Warn("metadata fetch group error", "error", err)
	}

	// Apply state updates sequentially.
	libs := l.Libraries()
	for i, v := range valid {
		media := results[i]
		if media == nil {
			continue
		}
		slog.Info("play event",
			"session", v.n.SessionKey, "user", v.sess.User.Title,
			"state", v.n.State, "title", media.Title,
			"offset", time.Duration(v.n.ViewOffset)*time.Millisecond)

		l.Sessions.Update(v.n.SessionKey, v.state, v.sess, media)
		l.Sessions.UpdateLibraryLabels(v.n.SessionKey, func(ss *sessions.Session) {
			FillSessionLibrary(ss, media, libs)
			MarkTranscodePending(ss, v.n.TranscodeSession)
		})
	}
}

// FillSessionLibrary populates library labels on ss when missing, using the
// provided library list matched by LibrarySectionID. No-op if ss already
// has a library name.
func FillSessionLibrary(ss *sessions.Session, media *plexapi.SessionMetadata, libs []library.Library) {
	if ss.LibName != "" {
		return
	}
	for _, lib := range libs {
		if lib.ID != media.LibrarySectionID.String() {
			continue
		}
		ss.LibName = lib.Name
		ss.LibID = lib.ID
		ss.LibType = lib.Type
		return
	}
}

// MarkTranscodePending records the transcode session key from the playing
// notification and flags the session as pending classification. The bare
// key (e.g. "pqa0kbrvowd7llgyggagzgjz") stored here is a suffix of the
// path-prefixed TranscodeSession.key (e.g. "/transcode/sessions/pqa0kbrvowd7llgyggagzgjz")
// that arrives on transcodeSession.update notifications, letting
// Tracker.UpdateTranscode correlate the two streams precisely even when
// multiple sessions are pending simultaneously. No-op if the session
// already has a resolved transcode kind.
func MarkTranscodePending(ss *sessions.Session, transcodeSession string) {
	if transcodeSession == "" || ss.TranscodeType != "" {
		return
	}
	ss.TranscodeType = metrics.ValPending
	ss.TranscodeKey = transcodeSession
}

// HandleTranscodeUpdate applies a "transcodeSession.update" websocket
// event by correlating it with an existing session (via stored bare
// transcode key or media part URL) and recording the classification.
func (l *Listener) HandleTranscodeUpdate(notif plexapi.WSNotification) {
	for i := range notif.NotificationContainer.TranscodeSession {
		ts := &notif.NotificationContainer.TranscodeSession[i]
		kind := TranscodeKind(ts)
		subtitle := SubtitleAction(ts)

		slog.Debug("transcode update", "key", ts.Key, "type", kind, "subtitle", subtitle)

		matched := l.Sessions.UpdateTranscode(ts.Key, kind, subtitle)

		if !matched {
			slog.Debug("transcode update unmatched", "key", ts.Key, "kind", kind)
		}
	}
}

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
