// Package metrics holds the Prometheus descriptor set
// (labels, descs, errorTypes allowlist) shared by the
// collector. It exports only descriptor variables; the
// Describe/Collect methods live on the Server type in
// internal/server.
package metrics
