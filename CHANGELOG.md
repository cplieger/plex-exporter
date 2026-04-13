# Changelog

## 2026.04.13-98ff0b3 (2026-04-13)

### Changed

- Refactor(plex-exporter): improve error handling, logging, and code organization
- Update Go toolchain configuration

### Dependencies

- Update go to v1.26.2
- Update golang:1.26-alpine docker digest to c2a1f7b

## 2026.04.07-d8d6bce (2026-04-08)

### Changed

- Update Go toolchain configuration

### Dependencies

- Update go to v1.26.2
- Update golang:1.26-alpine docker digest to c2a1f7b

## 2026.04.01-878c624 (2026-04-01)

### Added

- Enhance HTTP server security and consolidate response types
- Add nil check for response body before closing
- Test(plex-exporter): add property-based and edge case tests
- Migrate from gorilla to coder websocket library
- Enhance grafana dashboard layout and add websocket status
- Add custom prometheus exporter with grafana dashboard

### Fixed

- Enforce minimum TLS version for secure connections
- Improve library metrics aggregation in dashboard
- Improve library metrics query and preserve item counts
- Improve grafana dashboard metric queries

### Changed

- Refactor(plex-exporter): extract boolean string constants
- Refactor(plex-exporter): extract transcode kind string constants
- Refactor(plex-exporter): remove unused text config from dashboard panels
- Refactor(plex-exporter): minify grafana dashboard json and optimize main.go
- Update metrics port mapping to 9200

### Dependencies

- Update gcr.io/distroless/static-debian13:nonroot docker digest to e3f9456
- fix(deps): update module github.com/coder/websocket to v1.8.14
- fix(deps): update plex-exporter updates (#131)

## 2026.03.21-056dff5 (2026-03-22)

### Added

- Enhance HTTP server security and consolidate response types

## 2026.03.17-edc187a (2026-03-17)

### Fixed

- Enforce minimum TLS version for secure connections

## 2026.03.15-866f9fa (2026-03-16)

### Dependencies

- Update gcr.io/distroless/static-debian13:nonroot docker digest to e3f9456

## 2026.03.14-c258d2e (2026-03-14)

### Added

- Add nil check for response body before closing
- Test(plex-exporter): add property-based and edge case tests
- Migrate from gorilla to coder websocket library

### Changed

- Refactor(plex-exporter): extract boolean string constants
- Refactor(plex-exporter): extract transcode kind string constants

### Dependencies

- fix(deps): update module github.com/coder/websocket to v1.8.14

## 2026.03.13-142cbb3 (2026-03-13)

- Initial release
