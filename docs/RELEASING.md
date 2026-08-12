# Releasing

A release is a **promotion**, not a build. Work is staged on `next` until it is
ready, and `main` moves onto it — the images published as a release are the
staged images re-tagged, byte for byte, never a rebuild.

```
next  ──●──●──●──●─────────  candidates: 0.2.0-rc.1, -rc.2, -rc.3 …
         └── main ──────●     fast-forwarded onto a ready candidate, tagged v0.2.0
```

## The two branches

| Branch | What it means |
| --- | --- |
| `next` | The next release, staged. Every commit publishes a release candidate. |
| `main` | The live release. Only ever fast-forwarded onto a candidate. |

Every pull request targets `next`. Nothing is ever pushed to `main` directly —
not a hotfix, not a docs typo. Because `main` only fast-forwards, the two
branches can never diverge, so there is no cherry-picking and no merge to get
wrong: what is released is the identical commit that was staged.

## Versions

`charts/thunder-device-plugin/Chart.yaml` carries the version being **cooked**,
not the last one released. Bump it on `next` immediately after a promotion.

Candidates are numbered by distance from `main`, so the count restarts at each
promotion. `hack/release-version.sh` is the single source of that arithmetic;
CI never computes a version any other way.

```bash
make release-version     # 0.2.0-rc.3
```

The chart records the production `operator.image.tag` and `daemon.image.tag`
directly in `values.yaml`. Together with `Chart.yaml`, those values are the
release manifest for the exact component builds a chart installs.

## The loop

1. **Merge to `next`.** CI runs `make check` and the kind cluster test. On
   green, `release-candidate.yaml` builds and signs the images, publishes the
   chart to `oci://ghcr.io/thunder-compute/charts`, and cuts a prerelease
   `v0.2.0-rc.N`.

2. **Run the candidate.** Install it wherever the release is validated. A
   candidate is a real, immutable artifact, so whatever is tested is exactly
   what a release would ship.

3. **Fast-forward `main`** once the candidate is ready:

   ```bash
   git push origin v0.2.0-rc.3^{commit}:main
   ```

   This fails if it is not a fast-forward, which is exactly the protection
   wanted.

4. **Release.** Run the `release` workflow from `main` with the candidate
   version as input. It re-tags the candidate's image manifests — no rebuild —
   publishes the chart at the release version, verifies the digests match, and
   cuts `v0.2.0`.

5. **Bump `Chart.yaml`** on `next` to the next version.

Anyone pinned to the candidate can move to the release and see nothing change
but the image tag strings. Anything more means the release is not the artifact
that was staged, and the promotion is wrong.

## Hotfixes

Land on `next`, take a candidate, validate it, then fast-forward. Shipping a
fix straight to `main` skips the only evidence that it works.

## Checking a release by hand

```bash
make verify-promotion CANDIDATE=0.2.0-rc.3 RELEASE_VERSION=0.2.0

cosign verify ghcr.io/thunder-compute/thunder-device-plugin/operator:<tag> \
  --certificate-identity-regexp '^https://github.com/Thunder-Compute/thunder-device-plugin/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
