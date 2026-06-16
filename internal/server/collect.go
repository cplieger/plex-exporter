package server

import (
	"time"
	"unicode/utf8"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"github.com/prometheus/client_golang/prometheus"
)

// maxLabelLen is the maximum byte length for user-controlled Prometheus
// label values. Bounds cardinality from attacker-controlled Plex API
// strings.
const maxLabelLen = 128

// Describe implements prometheus.Collector. The descriptor set is the
// single source of truth published in metrics.AllDescs so that Describe
// and Collect cannot drift.
func (s *Server) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range metrics.AllDescs {
		ch <- d
	}
}

// Collect implements prometheus.Collector. It snapshots server state
// under s.mu (via Snapshot) and then emits metrics outside the lock so
// the collector never holds s.mu across a channel send.
func (s *Server) Collect(ch chan<- prometheus.Metric) {
	snap := s.Snapshot()

	ch <- prometheus.MustNewConstMetric(metrics.DescServerInfo, prometheus.GaugeValue, 1,
		snap.Name, snap.ID, snap.Version, snap.Platform, snap.PlatformVersion, snap.PlexPass)
	ch <- prometheus.MustNewConstMetric(metrics.DescHostCPU, prometheus.GaugeValue, snap.HostCPU, snap.Name, snap.ID)
	ch <- prometheus.MustNewConstMetric(metrics.DescHostMem, prometheus.GaugeValue, snap.HostMem, snap.Name, snap.ID)
	ch <- prometheus.MustNewConstMetric(metrics.DescTransmitBytes, prometheus.CounterValue, snap.TransmitBytes, snap.Name, snap.ID)
	ch <- prometheus.MustNewConstMetric(metrics.DescActiveTranscodes, prometheus.GaugeValue,
		float64(snap.ActiveTranscodes), snap.Name, snap.ID)
	ch <- prometheus.MustNewConstMetric(metrics.DescHTTPReachable, prometheus.GaugeValue, snap.HTTPReachable, snap.Name, snap.ID)

	// Emit one sample per known error type. Always emit all so
	// rate()/increase() return zero rather than stale values.
	for _, typ := range metrics.ErrorTypes {
		ch <- prometheus.MustNewConstMetric(metrics.DescErrors, prometheus.CounterValue,
			snap.ErrorCounts[typ], snap.Name, snap.ID, typ)
	}

	for _, lib := range snap.Libraries {
		ch <- prometheus.MustNewConstMetric(metrics.DescLibDuration, prometheus.GaugeValue,
			float64(lib.DurationTotal), snap.Name, snap.ID, lib.Type, lib.Name, lib.ID)
		ch <- prometheus.MustNewConstMetric(metrics.DescLibStorage, prometheus.GaugeValue,
			float64(lib.StorageTotal), snap.Name, snap.ID, lib.Type, lib.Name, lib.ID)
		if lib.ItemsCount > 0 {
			ch <- prometheus.MustNewConstMetric(metrics.DescLibItems, prometheus.GaugeValue,
				float64(lib.ItemsCount), snap.Name, snap.ID, lib.Type, lib.Name, lib.ID,
				library.ContentTypeLabel(lib.Type))
		}
	}
	s.collectSessions(ch, snap.Name, snap.ID, snap.Libraries)
}

// collectSessions emits per-session Prometheus metrics. Exported so
// tests in package main can call it directly through the embedded
// *Server; internal callers use Collect which invokes it after the
// server-level snapshot.
func (s *Server) collectSessions(ch chan<- prometheus.Metric, srvName, srvID string, libs []library.Library) {
	// Snapshot under lock so we can emit metrics without blocking
	// writers (handlePlaying, runPruneLoop, handleTranscodeUpdate)
	// behind a slow channel consumer. Matches the snapshot pattern
	// used by Collect() for s.mu.
	sessSnaps, estTotal := s.Sessions.SnapshotSessions()

	libByID := make(map[string]library.Library, len(libs))
	for _, l := range libs {
		libByID[l.ID] = l
	}

	for sessID := range sessSnaps {
		sess := sessSnaps[sessID]
		// Accumulate estimated transmit for playing sessions.
		if sess.State == sessions.StatePlaying && len(sess.Meta.Media) > 0 {
			estTotal += time.Since(sess.PlayStarted).Seconds() * float64(sess.Meta.Media[0].Bitrate)
		}

		if sess.PlayStarted.IsZero() {
			continue
		}

		labelVals := sessionLabelValues(srvName, srvID, &sess, sessID, libByID)

		ch <- prometheus.MustNewConstMetric(metrics.DescPlayCount, prometheus.GaugeValue, 1, labelVals...)

		totalPlay := sess.PrevPlayedTime
		if sess.State == sessions.StatePlaying {
			totalPlay += time.Since(sess.PlayStarted)
		}
		ch <- prometheus.MustNewConstMetric(metrics.DescPlaySeconds, prometheus.CounterValue,
			totalPlay.Seconds(), labelVals...)

		// Per-session bandwidth from the sessions API.
		if sess.Meta.Session.Bandwidth > 0 {
			ch <- prometheus.MustNewConstMetric(metrics.DescSessionBandwidth, prometheus.GaugeValue,
				float64(sess.Meta.Session.Bandwidth),
				srvName, srvID, sessID, truncLabel(sess.Meta.User.Title),
				truncLabel(orDefault(sess.Meta.Session.Location, metrics.ValUnknown)))
		}

		// Per-session bitrate (replaces the former stream_bitrate label).
		// Emitted as a gauge so cardinality is bounded by active session
		// count, not by the number of distinct bitrate values observed
		// during adaptive streaming. Value is 0 when Media is missing.
		if br := sessionBitrate(&sess.Meta); br > 0 {
			ch <- prometheus.MustNewConstMetric(metrics.DescSessionBitrate, prometheus.GaugeValue,
				br,
				srvName, srvID, sessID, truncLabel(sess.Meta.User.Title),
				truncLabel(orDefault(sess.Meta.Session.Location, metrics.ValUnknown)))
		}
	}

	// Estimated transmit bytes.
	ch <- prometheus.MustNewConstMetric(metrics.DescEstTransmitBytes, prometheus.CounterValue,
		estTotal*128, srvName, srvID)
}

// truncLabel truncates a label value to maxLabelLen bytes to prevent
// high-cardinality label sets from user-controlled Plex API data. It
// respects UTF-8 boundaries to avoid splitting multi-byte codepoints.
func truncLabel(s string) string {
	if len(s) <= maxLabelLen {
		return s
	}
	// Walk backward from the byte limit to find a valid UTF-8 boundary.
	i := maxLabelLen
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// streamLabels returns (streamType, resolution) derived from the
// live-playback Media info. Defaults are "unknown", "".
//
// The bitrate dimension was intentionally removed as a label (it caused
// unbounded cardinality because Plex reports changing bitrate values
// during adaptive streaming). See sessionBitrate() for the replacement
// gauge-valued metric emission.
func streamLabels(m *plexapi.SessionMetadata) (streamType, streamRes string) {
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
func sessionBitrate(m *plexapi.SessionMetadata) float64 {
	if len(m.Media) == 0 {
		return 0
	}
	return float64(m.Media[0].Bitrate)
}

// fileResolution returns the on-disk resolution (from library
// metadata), or empty string when not reported.
func fileResolution(m *plexapi.SessionMetadata) string {
	if len(m.Media) == 0 {
		return ""
	}
	return m.Media[0].VideoResolution
}

// resolveLibrary returns (name, id, type) using the session's cached
// values, falling back to a lookup in libByID, then to unknown.
func resolveLibrary(sess *sessions.Session, libByID map[string]library.Library) (name, id, typ string) {
	if sess.LibName != "" {
		return sess.LibName, sess.LibID, sess.LibType
	}
	if lb, ok := libByID[sess.MediaMeta.LibrarySectionID.String()]; ok {
		return lb.Name, lb.ID, lb.Type
	}
	return metrics.ValUnknown, "0", metrics.ValUnknown
}

// sessionLabelValues builds the Prometheus label value slice for a
// session. The slice order matches metrics.PlayLabels exactly.
func sessionLabelValues(
	srvName, srvID string,
	sess *sessions.Session, sessID string,
	libByID map[string]library.Library,
) []string {
	streamType, streamRes := streamLabels(&sess.Meta)
	fileRes := fileResolution(&sess.MediaMeta)
	libName, libID, libType := resolveLibrary(sess, libByID)

	title, childTitle, grandchildTitle := sessionLabels(&sess.MediaMeta)

	ttype := orDefault(sess.TranscodeType, metrics.ValNone)
	if ttype == metrics.ValPending {
		ttype = metrics.ValNone
	}
	local := metrics.ValFalse
	if sess.Meta.Player.Local {
		local = metrics.ValTrue
	}

	return []string{
		srvName, srvID, libName, libID, libType,
		metrics.MediaTypeAllowlist.Normalize(sess.MediaMeta.Type),
		truncLabel(title), truncLabel(childTitle), truncLabel(grandchildTitle),
		metrics.StreamTypeAllowlist.Normalize(streamType),
		metrics.ResolutionAllowlist.Normalize(streamRes),
		metrics.ResolutionAllowlist.Normalize(fileRes),
		truncLabel(sess.Meta.Player.Device), truncLabel(sess.Meta.Player.Product),
		truncLabel(sess.Meta.User.Title), sessID,
		ttype,
		orDefault(sess.SubtitleAction, metrics.ValNone),
		truncLabel(orDefault(sess.Meta.Session.Location, metrics.ValUnknown)),
		local,
	}
}

// sessionLabels returns (title, child, grandchild) picked from the
// session metadata based on the media type. For episodes and tracks
// the hierarchy is grandparent/parent/self; for movies only the title
// is used.
func sessionLabels(m *plexapi.SessionMetadata) (title, child, grandchild string) {
	switch m.Type {
	case library.TypeEpisode, library.TypeTrack:
		return m.GrandparentTitle, m.ParentTitle, m.Title
	default:
		return m.Title, "", ""
	}
}

// orDefault returns s when non-empty, otherwise def.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
