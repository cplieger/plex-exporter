package metrics

import (
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Order matters for the Prometheus wire contract; do not re-order.
var (
	srvLabels  = []string{LabelServer, LabelServerID}
	libLabels  = []string{LabelServer, LabelServerID, labelLibraryType, "library", labelLibraryID}
	playLabels = []string{
		LabelServer, LabelServerID,
		"library", labelLibraryID, labelLibraryType,
		"media_type", "title", "child_title", "grandchild_title", "grandchild_index",
		"stream_type", "stream_resolution", "stream_file_resolution",
		"device", "device_type", "user", "session",
		"transcode_type", "subtitle_action", "location", "local",
	}
)

// Canonical label values for Prometheus metrics. All packages must
// reference these instead of local string literals.
const (
	ValUnknown   = "unknown"
	ValNone      = "none"
	ValFalse     = "false"
	ValTrue      = "true"
	ValPending   = "pending"
	ValTranscode = "transcode"
	ValBurn      = "burn"
	ValCopy      = "copy"
	ValBoth      = "both"
	ValVideo     = "video"
	ValAudio     = "audio"

	LabelServer      = "server"
	LabelServerID    = "server_id"
	labelLibraryID   = "library_id"
	labelLibraryType = "library_type"
	FallbackOther    = "other"
)

// Prometheus descriptors emitted by the collector. Names, help text and
// label sets are the public metric contract; changing any of them is a
// breaking release.
var (
	DescServerInfo = prometheus.NewDesc(
		"plex_server_info", "Plex server information",
		append(srvLabels, "version", "platform", "platform_version", "plex_pass"), nil,
	)
	DescHostCPU = prometheus.NewDesc(
		"plex_host_cpu_utilization_ratio", "Host CPU utilization (0-1)",
		srvLabels, nil,
	)
	DescHostMem = prometheus.NewDesc(
		"plex_host_memory_utilization_ratio", "Host memory utilization (0-1)",
		srvLabels, nil,
	)
	DescLibDuration = prometheus.NewDesc(
		"plex_library_duration_milliseconds", "Total library duration in ms",
		libLabels, nil,
	)
	DescLibStorage = prometheus.NewDesc(
		"plex_library_storage_bytes", "Total library storage in bytes",
		libLabels, nil,
	)
	DescLibItems = prometheus.NewDesc(
		"plex_library_items", "Number of items in a library section",
		append(libLabels, "content_type"), nil,
	)
	DescTransmitBytes = prometheus.NewDesc(
		"plex_transmit_bytes_total", "Bytes transmitted (bandwidth API)",
		srvLabels, nil,
	)
	DescActiveTranscodes = prometheus.NewDesc(
		"plex_active_transcode_sessions", "Active transcode sessions",
		srvLabels, nil,
	)
	DescPlayCount = prometheus.NewDesc(
		"plex_plays_active", "Currently active play sessions (1 per session)",
		playLabels, nil,
	)
	DescPlaySeconds = prometheus.NewDesc(
		"plex_play_seconds_total", "Total play time per session",
		playLabels, nil,
	)
	DescSessionBandwidth = prometheus.NewDesc(
		"plex_session_bandwidth_kbps", "Session bandwidth in kbps",
		append(srvLabels, "session", "user", "location"), nil,
	)
	DescSessionBitrate = prometheus.NewDesc(
		"plex_session_bitrate_kbps",
		"Live stream bitrate per session (kbps). Replaces the former stream_bitrate label on plex_plays_active/plex_play_seconds_total, which caused unbounded cardinality as Plex reported changing bitrate values during adaptive streaming.",
		append(srvLabels, "session", "user", "location"), nil,
	)
	DescHTTPReachable = prometheus.NewDesc(
		"plex_http_reachable",
		"Whether the exporter's last poll of the configured Plex server succeeded (1) or failed (0)",
		nil, nil,
	)
	DescSessionPollReachable = prometheus.NewDesc(
		"plex_session_poll_reachable",
		"Whether the exporter's last /status/sessions poll succeeded (1) or failed (0)",
		nil, nil,
	)
	DescHTTPRetries = prometheus.NewDesc(
		"plex_http_retries_total",
		"HTTP retries the exporter's Plex client performed across all requests",
		nil, nil,
	)
	DescErrors = prometheus.NewDesc(
		"plex_exporter_errors_total",
		"Exporter errors by type",
		[]string{"type"}, nil,
	)
)

// libIDLabels identify a library without its free-text name, which the
// dashboard joins from plex_library_storage_bytes.
var libIDLabels = []string{LabelServer, LabelServerID, labelLibraryType, labelLibraryID}

// itemLabels are the identity labels of the top-N item families. Their
// active population is bounded by libstats.TopN, never by the catalog size.
var itemLabels = append(slices.Clone(libIDLabels), "rating_key", "title", "year")

func libDesc(name, help string, extra ...string) *prometheus.Desc {
	return prometheus.NewDesc(name, help, append(slices.Clone(libIDLabels), extra...), nil)
}

func srvDesc(name, help string, extra ...string) *prometheus.Desc {
	return prometheus.NewDesc(name, help, append(slices.Clone(srvLabels), extra...), nil)
}

// Library-content descriptors, published from the hourly catalog walk.
var (
	DescTopItemBytes = prometheus.NewDesc(
		"plex_library_top_item_bytes",
		"Size in bytes of one of the 10 largest items in a library (a show counts all its episodes)",
		itemLabels, nil,
	)
	DescTopItemLastPlayed = prometheus.NewDesc(
		"plex_library_top_item_last_played_timestamp_seconds",
		"Last play by any account of one of the 10 largest items, as a Unix time; 0 when no play is recorded",
		itemLabels, nil,
	)
	DescRecentItemAdded = prometheus.NewDesc(
		"plex_library_recent_item_added_timestamp_seconds",
		"When one of the 10 newest items on the server was added, as a Unix time",
		append(slices.Clone(itemLabels), "episode"), nil,
	)
	DescWatchAgeBytes = libDesc("plex_library_watch_age_bytes",
		"Bytes of sized items by when any account last played them", "last_watched")
	DescWatchAgeItems = libDesc("plex_library_watch_age_items",
		"Items by when any account last played them", "last_watched")
	DescResolutionBytes = libDesc("plex_library_resolution_bytes",
		"Bytes of sized items by video resolution", "video_resolution")
	DescCodecBytes = libDesc("plex_library_codec_bytes",
		"Bytes of sized items by video codec", "video_codec")
	DescMultiVersionItems = libDesc("plex_library_multi_version_items",
		"Items with more than one version (media file set)")
	DescExtraVersionBytes = libDesc("plex_library_extra_version_bytes",
		"Bytes held by every version of an item except its largest")
	DescNewestItemAdded = libDesc("plex_library_newest_item_added_timestamp_seconds",
		"When the newest item in the library was added, as a Unix time")
	DescAddedItems = libDesc("plex_library_added_items",
		"Items added within the window", "window")
	DescWalkable = libDesc("plex_library_walkable",
		"1 for each library the exporter reads item by item")
	DescLastScan = libDesc("plex_library_last_scan_timestamp_seconds",
		"When Plex last scanned the library, as a Unix time")
	DescWalkLastSuccess = libDesc("plex_library_walk_last_success_timestamp_seconds",
		"When the exporter last read every item of the library, as a Unix time")
	DescWalkDuration = libDesc("plex_library_walk_duration_seconds",
		"How long the last complete read of the library took")
	DescUnsizedItems = libDesc("plex_library_unsized_items",
		"Items whose file size Plex did not report for every version, left out of every byte figure")
	DescWalkPassDuration = srvDesc("plex_library_walk_pass_duration_seconds",
		"How long the last pass over every read library took")
	DescWalkSkipped = srvDesc("plex_library_walk_skipped_libraries",
		"Libraries past the limit of libraries the exporter reads item by item")
	DescWalkComplete = srvDesc("plex_library_walk_complete",
		"1 when the last pass read every library it covers and none was skipped")
	DescWatchAgeComplete = srvDesc("plex_library_watch_age_complete",
		"1 when every read library's watch-age figures come from current watch history with no unmatched rows")
)

// Watch-history, server-activity, update and transcode descriptors.
var (
	DescHistoryReadStatus = srvDesc("plex_history_read_status",
		"1 for the outcome of the last full watch-history read", "status")
	DescHistoryLastSuccess = srvDesc("plex_history_last_success_timestamp_seconds",
		"When the exporter last read watch history successfully, as a Unix time")
	DescHistoryUnmatched = srvDesc("plex_history_unmatched_rows",
		"Watch-history rows with a non-numeric item key, or an item key but no play time, since the last full read, counted up to 250,000")
	DescHistoryDeleted = srvDesc("plex_history_deleted_item_rows",
		"Watch-history rows for items no longer in Plex since the last full read, counted up to 250,000")
	DescActivityProgress = srvDesc("plex_server_activity_progress_ratio",
		"Progress of running Plex background work (0-1, -1 when Plex reports no estimate)",
		"activity_type", labelLibraryType, labelLibraryID)
	DescUpdateAvailable = srvDesc("plex_server_update_available",
		"1 when Plex reports a release newer than the running version", "state")
	DescUpdateChecked = srvDesc("plex_server_update_checked_timestamp_seconds",
		"When Plex last checked for an update, as a Unix time")
	DescSessionVideoTranscode = srvDesc("plex_session_video_transcode",
		"1 for each session transcoding video and 0 once it has ended, by where Plex decodes and encodes it",
		"session", "decode", "encode", "source_codec", "target_codec")
)

// AllDescs is the single source of truth for the descriptors emitted by
// Describe/Collect. Adding a descriptor here automatically extends the
// Describe-set test and keeps the two methods in sync.
var AllDescs = []*prometheus.Desc{
	DescServerInfo, DescHostCPU, DescHostMem,
	DescLibDuration, DescLibStorage, DescLibItems,
	DescTransmitBytes, DescActiveTranscodes,
	DescPlayCount, DescPlaySeconds,
	DescSessionBandwidth, DescSessionBitrate,
	DescHTTPReachable, DescSessionPollReachable, DescHTTPRetries, DescErrors,
	DescTopItemBytes, DescTopItemLastPlayed, DescRecentItemAdded,
	DescWatchAgeBytes, DescWatchAgeItems, DescResolutionBytes, DescCodecBytes,
	DescMultiVersionItems, DescExtraVersionBytes, DescNewestItemAdded, DescAddedItems,
	DescWalkable, DescLastScan, DescWalkLastSuccess, DescWalkDuration, DescUnsizedItems,
	DescWalkPassDuration, DescWalkSkipped, DescWalkComplete, DescWatchAgeComplete,
	DescHistoryReadStatus, DescHistoryLastSuccess, DescHistoryUnmatched, DescHistoryDeleted,
	DescActivityProgress, DescUpdateAvailable, DescUpdateChecked,
	DescSessionVideoTranscode,
}

// ErrorTypes is the bounded allowlist of `type` label values emitted on
// DescErrors. Keeping it fixed prevents unbounded Prometheus cardinality
// from a compromised Plex server returning attacker-controlled error strings.
var ErrorTypes = []string{
	"refresh", "sessions_fetch", "metadata_fetch",
	"invalid_rating_key", "metrics_server", "library_items",
	"library_walk", "history_fetch", "activities_fetch", "sections_fetch", "updater_fetch",
}

// LabelAllowlist defines a bounded set of valid Prometheus label values.
// Unknown values are normalised to Fallback to prevent cardinality explosion.
type LabelAllowlist struct {
	Allowed  map[string]bool
	Fallback string
}

// Normalize returns the lowercased value when it is in the allowlist,
// otherwise the allowlist's Fallback value.
func (a *LabelAllowlist) Normalize(v string) string {
	low := strings.ToLower(v)
	if a.Allowed[low] {
		return low
	}
	return a.Fallback
}

// Declarative allowlists for Prometheus label cardinality bounding.
var (
	StreamTypeAllowlist = &LabelAllowlist{
		Allowed:  map[string]bool{ValCopy: true, ValTranscode: true, "directplay": true, ValUnknown: true},
		Fallback: FallbackOther,
	}
	MediaTypeAllowlist = &LabelAllowlist{
		Allowed:  map[string]bool{"movie": true, "episode": true, "track": true, "clip": true, "photo": true},
		Fallback: FallbackOther,
	}
	ResolutionAllowlist = &LabelAllowlist{
		Allowed:  map[string]bool{"": true, "sd": true, "480": true, "576": true, "720": true, "1080": true, "4k": true, "2160": true},
		Fallback: FallbackOther,
	}
	LocationAllowlist = &LabelAllowlist{
		Allowed:  map[string]bool{"lan": true, "wan": true, ValUnknown: true},
		Fallback: FallbackOther,
	}
)

// videoCodecAllowlist bounds video_codec, source_codec and target_codec.
// An empty codec is unknown, not other: Plex sent none.
var videoCodecAllowlist = &LabelAllowlist{
	Allowed: map[string]bool{
		"h264": true, "hevc": true, "av1": true, "vp9": true,
		"mpeg2video": true, "mpeg4": true, "vc1": true,
	},
	Fallback: FallbackOther,
}

// NormalizeCodec returns a Plex codec name as a lowercased known video codec,
// "other" for an unlisted one, or "unknown" when it is empty.
func NormalizeCodec(v string) string {
	if strings.TrimSpace(v) == "" {
		return ValUnknown
	}
	return videoCodecAllowlist.Normalize(strings.TrimSpace(v))
}

// ResolutionBucket maps a Plex videoResolution onto the closed set
// sd, 480, 576, 720, 1080, 2160, other, unknown; "4k" is 2160.
func ResolutionBucket(v string) string {
	switch low := strings.ToLower(strings.TrimSpace(v)); low {
	case "":
		return ValUnknown
	case "4k", "2160":
		return "2160"
	case "sd", "480", "576", "720", "1080":
		return low
	default:
		return FallbackOther
	}
}
