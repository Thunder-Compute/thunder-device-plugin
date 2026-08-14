# Changelog

Notable changes to the chart and its components. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Release candidates (`0.2.0-rc.N`) are not listed separately; they become the
release they were promoted to. See [docs/RELEASING.md](docs/RELEASING.md).

## [Unreleased]

### Changed

- Images are published to the canonical nested repositories under
  `ghcr.io/thunder-compute/thunder-device-plugin`.
- CI records the published source-commit image tag in `values.yaml`, so the
  chart commit pins exactly what runs.

### Added

- `image.digest` for both components, pinning past the tag when set.
