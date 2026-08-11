# Changelog

Notable changes to the chart and its components. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Release candidates (`0.2.0-rc.N`) are not listed separately; they become the
release they were promoted to. See [docs/RELEASING.md](docs/RELEASING.md).

## [Unreleased]

### Changed

- Images are published to `ghcr.io/thunder-compute` and the chart's image tags
  default to `appVersion`, so a chart version pins exactly what runs.
- Images and chart are signed with cosign and published with SBOM and
  provenance attestations.

### Added

- `image.digest` for both components, pinning past the tag when set.
