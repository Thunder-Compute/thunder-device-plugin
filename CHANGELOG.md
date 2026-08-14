# Changelog

Notable changes to the chart and its components. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Release candidates (`0.2.0-rc.N`) are not listed separately; they become the
release they were promoted to. See [docs/RELEASING.md](docs/RELEASING.md).

## [Unreleased]

### Changed

- Images are published under `ghcr.io/thunder-compute/thunder-device-plugin`,
  and each chart release records its tested image tags in `values.yaml`.

### Added

- `image.digest` for both components, pinning past the tag when set.
- `thunder.portRange`, declaring the host data ports thunderd binds on each
  enrolled node. It defaults to the installer's regular `32000-32199` range and
  can be set to a cluster-specific range such as `61000-61199`.
