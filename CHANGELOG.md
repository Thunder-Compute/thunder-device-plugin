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
- **The host data ports thunderd binds moved to `61000-61199`.** Nodes used to
  take the Thunder installer's own default, `32000-32199`, which sits inside
  Kubernetes' default NodePort range (`30000-32767`) — an overlap that misroutes
  CUDA traffic without either side reporting a conflict, because under Cilium's
  eBPF kube-proxy replacement a NodePort has no listening socket for `bind()` to
  fail on. The new default clears both the NodePort range and the kernel's
  ephemeral range (`32768-60999`), so the chart is safe to install on an
  unmodified cluster, managed EKS/GKE/AKS included.

  This is a behaviour change for existing installs. The range is applied at
  enrollment, so an already-enrolled node keeps the range it was installed with
  and picks up the new one only when it re-enrolls. **Open `61000-61199` in the
  firewalls and security groups in front of your nodes before they re-enrol**,
  or a re-enrolled node becomes unreachable for CUDA traffic. Setting
  `thunder.portRange: "32000-32199"` keeps the old range.

### Added

- `image.digest` for both components, pinning past the tag when set.
- `thunder.portRange`, declaring the host data ports thunderd binds on each
  enrolled node. See the default change above; `""` still hands the choice back
  to the Thunder installer.
