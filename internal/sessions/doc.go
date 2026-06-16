// Package sessions maintains the in-memory active-session tracker
// updated by the session poll loop and snapshotted by the collector.
// The tracker exposes its mutex, map, and per-session fields so the
// remaining callers in package main can orchestrate lock scope without
// a wall of getter methods; when a caller outside this module needs a
// narrower contract a later cycle can add Snapshot / Apply helpers.
//
// Exported symbols: State / StatePlaying / StateStopped, the
// SessionTimeout / StaleSessionTimeout / MaxSessionKeyLen /
// MaxTrackedSessions bounds, the Session DTO, Tracker with NewTracker,
// Update, Prune, and RunPruneLoop.
package sessions
