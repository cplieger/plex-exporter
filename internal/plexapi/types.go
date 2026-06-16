package plexapi

import "encoding/json"

// MC is the generic Plex media-container envelope. Most Plex JSON
// responses wrap the real payload inside a top-level "MediaContainer"
// object, so callers decode into MC[ConcretePayload].
type MC[T any] struct {
	MediaContainer T `json:"MediaContainer"`
}

// ServerIdentity models /identity and /:/resources responses.
type ServerIdentity struct {
	FriendlyName                  string `json:"friendlyName"`
	MachineIdentifier             string `json:"machineIdentifier"`
	Version                       string `json:"version"`
	Platform                      string `json:"platform"`
	PlatformVersion               string `json:"platformVersion"`
	MyPlexSubscription            bool   `json:"myPlexSubscription"`
	TranscoderActiveVideoSessions int    `json:"transcoderActiveVideoSessions"`
}

// MediaProviderResponse models /media/providers.
type MediaProviderResponse struct {
	FriendlyName      string `json:"friendlyName"`
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
	MediaProviders    []struct {
		Identifier string `json:"identifier"`
		Features   []struct {
			Type        string `json:"type"`
			Directories []struct {
				Title         string `json:"title"`
				ID            string `json:"id"`
				Type          string `json:"type"`
				DurationTotal int64  `json:"durationTotal"`
				StorageTotal  int64  `json:"storageTotal"`
			} `json:"Directory"`
		} `json:"Feature"`
	} `json:"MediaProvider"`
}

// StatisticsResource models /statistics/resources entries.
type StatisticsResource struct {
	HostCPUUtil float64 `json:"hostCpuUtilization"`
	HostMemUtil float64 `json:"hostMemoryUtilization"`
}

// StatisticsBandwidth models /statistics/bandwidth entries.
type StatisticsBandwidth struct {
	Bytes int64 `json:"bytes"`
	At    int   `json:"at"`
}

// MediaPart describes a single part of a Media entry (video/audio stream
// decision + key).
type MediaPart struct {
	Decision string `json:"decision"`
	Key      string `json:"key"`
}

// MediaInfo describes the Media element attached to a session.
type MediaInfo struct {
	VideoResolution string      `json:"videoResolution"`
	VideoCodec      string      `json:"videoCodec"`
	AudioCodec      string      `json:"audioCodec"`
	Part            []MediaPart `json:"Part"`
	Bitrate         int         `json:"bitrate"`
}

// SessionMetadata models an entry in /status/sessions or in
// /library/metadata/<id>.
type SessionMetadata struct {
	TranscodeSession *WSTranscodeSession `json:"TranscodeSession"`
	User             struct {
		Title string `json:"title"`
		ID    string `json:"id"`
	} `json:"User"`
	GrandparentTitle string      `json:"grandparentTitle"`
	ParentTitle      string      `json:"parentTitle"`
	Title            string      `json:"title"`
	Type             string      `json:"type"`
	LibrarySectionID json.Number `json:"librarySectionID"`
	SessionKey       string      `json:"sessionKey"`
	RatingKey        string      `json:"ratingKey"`
	Player           struct {
		Device  string `json:"device"`
		Product string `json:"product"`
		State   string `json:"state"`
		Local   bool   `json:"local"`
		Relayed bool   `json:"relayed"`
		Secure  bool   `json:"secure"`
	} `json:"Player"`
	Session struct {
		Location  string `json:"location"`
		Bandwidth int    `json:"bandwidth"`
	} `json:"Session"`
	Media []MediaInfo `json:"Media"`
}

// MetadataListResponse is shared by /status/sessions and
// /library/metadata/<id>.
type MetadataListResponse struct {
	Metadata []SessionMetadata `json:"Metadata"`
}

// WSTranscodeSession is a TranscodeSession element embedded in the
// /status/sessions response (nested under each Video entry). The
// classification functions in package sessions use its decision fields
// to derive the transcode_type and subtitle_action Prometheus labels.
// The "WS" prefix is historical (the struct originated from the
// websocket notification path); it is kept to avoid a rename-churn
// across consumers.
type WSTranscodeSession struct {
	Key              string `json:"key"`
	VideoDecision    string `json:"videoDecision"`
	AudioDecision    string `json:"audioDecision"`
	SubtitleDecision string `json:"subtitleDecision"`
	SourceVideoCodec string `json:"sourceVideoCodec"`
	SourceAudioCodec string `json:"sourceAudioCodec"`
	VideoCodec       string `json:"videoCodec"`
	AudioCodec       string `json:"audioCodec"`
	Container        string `json:"container"`
}
