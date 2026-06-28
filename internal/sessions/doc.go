// Package sessions maintains the in-memory active-session tracker
// updated by the session poll loop and snapshotted by the collector.
// The tracker exposes its map and per-session fields (the mutex stays
// unexported) so package server can orchestrate lock scope without a wall
// of getter methods; SnapshotSessions and UpdateLibraryLabels are the
// snapshot/apply helpers callers use for lock-safe access.
//
// Exported symbols: State with ParseState and the StatePlaying /
// StateStopped / StatePaused / StateOther constants; the MaxSessionKeyLen /
// MaxTrackedSessions bounds; the Session DTO; PruneConfig; the transcode
// classifiers TranscodeKind and SubtitleAction; and Tracker with NewTracker,
// Update, UpdateLibraryLabels, MarkAbsentStopped, SnapshotSessions, Prune,
// and RunPruneLoop.
package sessions
