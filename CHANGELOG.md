# Changelog

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
