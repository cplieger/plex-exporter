// Package plex provides the HTTP client used to talk to a
// Plex Media Server, including retry semantics and the ErrNotFound
// sentinel used by the Plex Pass graceful-degradation path; status-code
// classification uses the shared plexapi library's StatusError directly.
package plex
