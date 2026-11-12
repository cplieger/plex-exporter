package server

import (
	"context"
	"strconv"
	"time"

	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"github.com/cplieger/plexapi/v3"
)

// maxLabelLen is the maximum byte length for user-controlled Prometheus
// label values. Bounds cardinality from attacker-controlled Plex API
// strings.
const maxLabelLen = 128

// Collector returns a new collector that reads s at every scrape and serves
// version as plex_exporter_build_info.
func (s *Server) Collector(version string) *measure.Collector {
	return newCollector(func(em *emitter) {
		em.emit(metrics.DescBuildInfo, 1, version)
		s.collect(em)
	})
}

// newCollector declares every family in metrics.AllDescs for one collector
// that runs collect at every scrape.
func newCollector(collect func(*emitter)) *measure.Collector {
	descs := make(map[*metrics.Desc]*measure.Desc, len(metrics.AllDescs))
	declared := make([]*measure.Desc, 0, len(metrics.AllDescs))
	for _, d := range metrics.AllDescs {
		md := d.Declare()
		descs[d] = md
		declared = append(declared, md)
	}
	return measure.NewCollector(func(_ context.Context, e *measure.Emitter) {
		collect(&emitter{e: e, descs: descs})
	}, declared...)
}

// emitter writes one scrape's samples under the families its collector
// declared.
type emitter struct {
	e     *measure.Emitter
	descs map[*metrics.Desc]*measure.Desc
}

func (em *emitter) emit(d *metrics.Desc, v float64, labels ...string) {
	em.e.Emit(em.descs[d], v, labels...)
}

func (s *Server) collect(em *emitter) {
	snap := s.snapshot()

	em.emit(metrics.DescHTTPReachable, snap.HTTPReachable)
	em.emit(metrics.DescSessionPollReachable, snap.SessionsReachable)
	em.emit(metrics.DescHTTPRetries, snap.Retries)
	// Emit one sample per known error type and fetch so rate()/increase()
	// return zero rather than stale values.
	for _, typ := range metrics.ErrorTypes {
		em.emit(metrics.DescErrors, snap.ErrorCounts[typ], typ)
	}
	for _, fetch := range metrics.RefreshFetches {
		em.emit(metrics.DescRefreshDeadlineExceeded, snap.deadlineMisses[fetch], fetch)
	}
	em.emit(metrics.DescSessionsTracked, float64(snap.sessionsTracked))
	em.emit(metrics.DescSessionsDropped, snap.sessionsDropped)

	// Prometheus drops an empty label value, so a server-scoped sample emitted
	// before Plex has answered lands on a different series than the healthy one.
	if snap.ID == "" {
		return
	}

	em.emit(metrics.DescServerInfo, 1,
		snap.Name, snap.ID, snap.Version, snap.Platform, snap.PlatformVersion, snap.PlexPass)
	em.emit(metrics.DescActiveTranscodes, float64(snap.ActiveTranscodes), snap.Name, snap.ID)
	if snap.ResourcesRead {
		em.emit(metrics.DescHostCPU, snap.HostCPU, snap.Name, snap.ID)
		em.emit(metrics.DescHostMem, snap.HostMem, snap.Name, snap.ID)
	}
	if snap.BandwidthRead {
		em.emit(metrics.DescTransmitBytes, snap.TransmitBytes, snap.Name, snap.ID)
	}

	for _, lib := range snap.Libraries {
		libName := truncLabel(lib.Name)
		em.emit(metrics.DescLibDuration, float64(lib.DurationTotal), snap.Name, snap.ID, lib.Type, libName, lib.ID)
		em.emit(metrics.DescLibStorage, float64(lib.StorageTotal), snap.Name, snap.ID, lib.Type, libName, lib.ID)
		if lib.ItemsKnown {
			em.emit(metrics.DescLibItems, float64(lib.ItemsCount), snap.Name, snap.ID, lib.Type, libName, lib.ID,
				library.ContentTypeLabel(lib.Type))
		}
	}
	s.collectSessions(em, snap.Name, snap.ID, snap.Libraries)
	collectCatalog(em, &snap)
}

// collectSessions emits per-session metrics from a sessions-only snapshot
// taken under the tracker lock (collect already took the server-level
// snapshot).
func (s *Server) collectSessions(em *emitter, srvName, srvID string, libs []library.Library) {
	sessSnaps := s.Sessions.SnapshotSessions()

	libByID := make(map[string]library.Library, len(libs))
	for _, l := range libs {
		libByID[l.ID] = l
	}

	for sessID := range sessSnaps {
		sess := sessSnaps[sessID]
		if sess.PlayStarted.IsZero() {
			continue
		}

		labelVals := sessionLabelValues(srvName, srvID, &sess, sessID, libByID)

		em.emit(metrics.DescPlayCount, 1, labelVals...)

		totalPlay := sess.PrevPlayedTime
		if sess.State == sessions.StatePlaying {
			totalPlay += time.Since(sess.PlayStarted)
		}
		em.emit(metrics.DescPlaySeconds, totalPlay.Seconds(), labelVals...)
		if sess.VideoTranscoding {
			// An ended session reads 0 while it waits to be pruned, so its
			// row keeps the pipeline but it adds no transcode time.
			transcoding := 1.0
			if sess.State == sessions.StateStopped {
				transcoding = 0
			}
			em.emit(metrics.DescSessionVideoTranscode, transcoding,
				srvName, srvID, sessID, sess.VideoDecode, sess.VideoEncode, sess.SourceVideoCodec, sess.TargetVideoCodec)
		}

		user, _, bw := sessionElements(&sess.Meta)

		if bw.Bandwidth > 0 {
			em.emit(metrics.DescSessionBandwidth, float64(bw.Bandwidth),
				srvName, srvID, sessID, truncLabel(user.Title),
				metrics.LocationAllowlist.Normalize(orDefault(bw.Location, metrics.ValUnknown)))
		}

		// Bitrate is a gauge (not a play-count label) so cardinality is bounded
		// by session count, not by every adaptive-streaming bitrate value seen.
		if br := sessionBitrate(&sess.Meta); br > 0 {
			em.emit(metrics.DescSessionBitrate, br,
				srvName, srvID, sessID, truncLabel(user.Title),
				metrics.LocationAllowlist.Normalize(orDefault(bw.Location, metrics.ValUnknown)))
		}
	}
}

// truncLabel bounds a label value to maxLabelLen bytes, capping Prometheus
// cardinality from user-controlled Plex API strings.
func truncLabel(s string) string {
	return textsafe.CapBytes(s, maxLabelLen)
}

// streamLabels returns (streamType, resolution) derived from the
// live-playback Media info. Defaults are "unknown", "".
//
// Bitrate is deliberately not a label here (Plex reports changing bitrate
// during adaptive streaming, causing unbounded cardinality); see sessionBitrate.
func streamLabels(m *plexapi.Item) (streamType, streamRes string) {
	streamType = metrics.ValUnknown
	if len(m.Media) == 0 {
		return streamType, ""
	}
	media := m.Media[0]
	if len(media.Part) > 0 && media.Part[0].Decision != "" {
		streamType = media.Part[0].Decision
	}
	return streamType, media.VideoResolution
}

// sessionBitrate returns the session's live-stream bitrate in kbps, or
// 0 when Media is missing or reports no bitrate. Emitted as the
// plex_session_bitrate_kbps gauge (bounded by active session count).
func sessionBitrate(m *plexapi.Item) float64 {
	if len(m.Media) == 0 {
		return 0
	}
	return float64(m.Media[0].Bitrate)
}

// fileResolution returns the on-disk resolution (from library
// metadata), or empty string when not reported.
func fileResolution(m *plexapi.Item) string {
	if len(m.Media) == 0 {
		return ""
	}
	return m.Media[0].VideoResolution
}

// sessionElements returns a session item's User, Player and Session elements,
// substituting zero values for the ones Plex omitted. The collector's
// contract is to degrade to unknown/empty labels, never to panic mid-scrape.
func sessionElements(it *plexapi.Item) (plexapi.SessionUser, plexapi.Player, plexapi.SessionBandwidth) {
	var user plexapi.SessionUser
	var player plexapi.Player
	var bw plexapi.SessionBandwidth
	if it.User != nil {
		user = *it.User
	}
	if it.Player != nil {
		player = *it.Player
	}
	if it.Session != nil {
		bw = *it.Session
	}
	return user, player, bw
}

// resolveLibrary returns (name, id, type) using the session's cached
// values, falling back to a lookup in libByID, then to unknown.
func resolveLibrary(sess *sessions.Session, libByID map[string]library.Library) (name, id, typ string) {
	if sess.LibName != "" {
		return sess.LibName, sess.LibID, sess.LibType
	}
	if lb, ok := libByID[strconv.Itoa(int(sess.MediaMeta.LibrarySectionID))]; ok {
		return lb.Name, lb.ID, lb.Type
	}
	return metrics.ValUnknown, "0", metrics.ValUnknown
}

// sessionLabelValues builds the Prometheus label value slice for a
// session. The slice order matches the playback descriptors' label order exactly.
func sessionLabelValues(
	srvName, srvID string,
	sess *sessions.Session, sessID string,
	libByID map[string]library.Library,
) []string {
	streamType, streamRes := streamLabels(&sess.Meta)
	fileRes := fileResolution(&sess.MediaMeta)
	libName, libID, libType := resolveLibrary(sess, libByID)

	title, childTitle, grandchildTitle := sessionLabels(&sess.MediaMeta)
	grandchildIndex := sessionIndex(&sess.MediaMeta)

	ttype := orDefault(sess.TranscodeType, metrics.ValNone)
	if ttype == metrics.ValPending {
		ttype = metrics.ValNone
	}
	user, player, bw := sessionElements(&sess.Meta)
	local := metrics.ValFalse
	if player.Local {
		local = metrics.ValTrue
	}

	return []string{
		srvName, srvID, truncLabel(libName), libID, libType,
		metrics.MediaTypeAllowlist.Normalize(sess.MediaMeta.Type),
		truncLabel(title), truncLabel(childTitle), truncLabel(grandchildTitle), grandchildIndex,
		metrics.StreamTypeAllowlist.Normalize(streamType),
		metrics.ResolutionAllowlist.Normalize(streamRes),
		metrics.ResolutionAllowlist.Normalize(fileRes),
		truncLabel(player.Device), truncLabel(player.Product),
		truncLabel(user.Title), sessID,
		ttype,
		orDefault(sess.SubtitleAction, metrics.ValNone),
		metrics.LocationAllowlist.Normalize(orDefault(bw.Location, metrics.ValUnknown)),
		local,
	}
}

// sessionLabels returns (title, child, grandchild) picked from the
// session metadata based on the media type. For episodes and tracks
// the hierarchy is grandparent/parent/self; for movies only the title
// is used.
func sessionLabels(m *plexapi.Item) (title, child, grandchild string) {
	switch m.Type {
	case library.TypeEpisode, library.TypeTrack:
		return m.GrandparentTitle, m.ParentTitle, m.Title
	default:
		return m.Title, "", ""
	}
}

// sessionIndex returns the episode or track number as a string for
// episodes and tracks, or "" for movies and other types where Plex's
// index field is not meaningful. Kept as a separate label
// (grandchild_index) so grandchild_title continues to carry the name.
func sessionIndex(m *plexapi.Item) string {
	switch m.Type {
	case library.TypeEpisode, library.TypeTrack:
		if m.Index > 0 {
			return strconv.Itoa(int(m.Index))
		}
	}
	return ""
}

// orDefault returns s when non-empty, otherwise def.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
