// Package plex provides the HTTP client used to talk to a
// Plex Media Server, including retry semantics, the ErrNotFound
// sentinel, and the HTTPStatusError type used by callers to
// distinguish 4xx from 5xx responses.
package plex
