package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Label slice vars shared across descriptors. Order matters for the
// Prometheus wire contract (inviolate contract #6/#7); do not re-order.
var (
	SrvLabels  = []string{LabelServer, LabelServerID}
	LibLabels  = []string{LabelServer, LabelServerID, "library_type", "library", "library_id"}
	PlayLabels = []string{
		LabelServer, LabelServerID,
		"library", "library_id", "library_type",
		"media_type", "title", "child_title", "grandchild_title", "grandchild_index",
		"stream_type", "stream_resolution", "stream_file_resolution",
		"device", "device_type", "user", "session",
		"transcode_type", "subtitle_action", "location", "local",
	}
)

// Canonical label values for Prometheus metrics (inviolate contract #6).
// All packages must reference these instead of local string literals.
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

	LabelServer   = "server"
	LabelServerID = "server_id"
	FallbackOther = "other"
)

// Prometheus descriptors emitted by the collector. Names, help text, and
// label sets are part of the public metric contract and must not change.
var (
	DescServerInfo = prometheus.NewDesc(
		"plex_server_info", "Plex server information",
		append(SrvLabels, "version", "platform", "platform_version", "plex_pass"), nil)
	DescHostCPU = prometheus.NewDesc(
		"plex_host_cpu_utilization_ratio", "Host CPU utilization (0-1)",
		SrvLabels, nil)
	DescHostMem = prometheus.NewDesc(
		"plex_host_memory_utilization_ratio", "Host memory utilization (0-1)",
		SrvLabels, nil)
	DescLibDuration = prometheus.NewDesc(
		"plex_library_duration_milliseconds", "Total library duration in ms",
		LibLabels, nil)
	DescLibStorage = prometheus.NewDesc(
		"plex_library_storage_bytes", "Total library storage in bytes",
		LibLabels, nil)
	DescLibItems = prometheus.NewDesc(
		"plex_library_items", "Number of items in a library section",
		append(LibLabels, "content_type"), nil)
	DescTransmitBytes = prometheus.NewDesc(
		"plex_transmit_bytes_total", "Bytes transmitted (bandwidth API)",
		SrvLabels, nil)
	DescActiveTranscodes = prometheus.NewDesc(
		"plex_active_transcode_sessions", "Active transcode sessions",
		SrvLabels, nil)
	DescPlayCount = prometheus.NewDesc(
		"plex_plays_active", "Currently active play sessions (1 per session)",
		PlayLabels, nil)
	DescPlaySeconds = prometheus.NewDesc(
		"plex_play_seconds_total", "Total play time per session",
		PlayLabels, nil)
	DescSessionBandwidth = prometheus.NewDesc(
		"plex_session_bandwidth_kbps", "Session bandwidth in kbps",
		append(SrvLabels, "session", "user", "location"), nil)
	DescSessionBitrate = prometheus.NewDesc(
		"plex_session_bitrate_kbps",
		"Live stream bitrate per session (kbps). Replaces the former stream_bitrate label on plex_plays_active/plex_play_seconds_total, which caused unbounded cardinality as Plex reported changing bitrate values during adaptive streaming.",
		append(SrvLabels, "session", "user", "location"), nil)
	DescEstTransmitBytes = prometheus.NewDesc(
		"plex_estimated_transmit_bytes_total", "Estimated bytes from bitrates",
		SrvLabels, nil)
	DescHTTPReachable = prometheus.NewDesc(
		"plex_http_reachable", "HTTP polling reachability (1=last refresh succeeded, 0=failed)",
		SrvLabels, nil)
	DescSessionPollReachable = prometheus.NewDesc(
		"plex_session_poll_reachable", "Session poll reachability (1=last /status/sessions poll succeeded, 0=failed)",
		SrvLabels, nil)
	DescErrors = prometheus.NewDesc(
		"plex_exporter_errors_total",
		"Plex exporter error count by type",
		append(SrvLabels, "type"), nil)
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
	DescEstTransmitBytes,
	DescHTTPReachable, DescSessionPollReachable, DescErrors,
}

// ErrorTypes is the bounded allowlist of `type` label values emitted on
// DescErrors. Keeping it fixed prevents unbounded Prometheus cardinality
// from a compromised Plex server returning attacker-controlled error strings.
var ErrorTypes = []string{
	"refresh", "sessions_fetch", "metadata_fetch",
	"invalid_rating_key", "metrics_server", "library_items",
}

// LabelAllowlist defines a bounded set of valid Prometheus label values.
// Unknown values are normalised to Fallback to prevent cardinality explosion.
type LabelAllowlist struct {
	Name     string
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
		Name:     "stream_type",
		Allowed:  map[string]bool{"copy": true, "transcode": true, "directplay": true, ValUnknown: true},
		Fallback: FallbackOther,
	}
	MediaTypeAllowlist = &LabelAllowlist{
		Name:     "media_type",
		Allowed:  map[string]bool{"movie": true, "episode": true, "track": true, "clip": true, "photo": true},
		Fallback: FallbackOther,
	}
	ResolutionAllowlist = &LabelAllowlist{
		Name:     "resolution",
		Allowed:  map[string]bool{"": true, "sd": true, "480": true, "576": true, "720": true, "1080": true, "4k": true, "2160": true},
		Fallback: FallbackOther,
	}
)
