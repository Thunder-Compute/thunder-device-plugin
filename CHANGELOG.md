# Changelog

Notable changes to the chart and its components. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Release candidates (`0.2.0-rc.N`) are not listed separately; they become the
release they were promoted to. See [docs/RELEASING.md](docs/RELEASING.md).

## [Unreleased]

### Fixed

- The daemon no longer reinstalls thunderd on a healthy node. `thunder status`
  reports a transiently installed thunderd as unhealthy because a transient
  unit is never `enabled`, and the daemon believed it: every pass re-downloaded
  the Thunder CLI and spent a fresh enrollment token on a node that was already
  enrolled and serving. Health is now read from what thunderd answers — its
  local API and its auth token — rather than from any status or systemd state
  string.
- A node that is enrolled but not running is restarted with the auth token it
  already has, instead of being reinstalled and enrolled again. The daemon
  falls back to reinstalling when restarting does not work.

### Added

- Daemon pods republish thunderd's journal as their own log under a `thunderd:`
  prefix, so a node problem is readable with `kubectl logs`. `THUNDERD_LOG_UNIT`
  names the unit, or turns the stream off.
- `image.digest` for both components, pinning past the tag when set.

### Changed

- thunderd status is logged when it changes rather than on every reconcile
  pass, and installer output is timestamped and attributed like every other
  line in the pod log.
- Images are published to the canonical nested repositories under
  `ghcr.io/thunder-compute/thunder-device-plugin`.
- CI records the published source-commit image tag in `values.yaml`, so the
  chart commit pins exactly what runs.
