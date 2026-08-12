# Releasing

A chart release is a **composition**, not an image build. Work is staged on
`next` until it is ready, and `main` moves onto it. The daemon and operator are
published independently with UTC timestamp tags; `values.yaml` records the
exact builds composed by the chart.

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

1. **Publish component builds as needed.** Use one UTC timestamp for both
   images when both components change. Test those exact images, then record
   their tags in `values.yaml`. A chart-only change reuses the existing tags.

2. **Merge to `next`.** CI checks the code and chart, verifies that the images
   selected by `values.yaml` exist, and cuts source prerelease `v0.2.0-rc.N`.
   Validate the chart from that exact commit with those exact images.

3. **Fast-forward `main`** once the candidate is ready:

   ```bash
   git push origin v0.2.0-rc.3^{commit}:main
   ```

   This fails if it is not a fast-forward, which is exactly the protection
   wanted.

4. **Release.** Run the `release` workflow from `main` with the candidate
   version as input. It verifies the timestamped images still exist, publishes
   the version from `Chart.yaml`, and cuts GitHub Release `v0.2.0`. It never
   builds or re-tags a component image.

5. **Bump `Chart.yaml`** on `next` to the next version.

The chart package, its source tag, and the two image tags in `values.yaml`
together identify the release. Published timestamp image tags are immutable
and must remain available while any chart release references them.

## Hotfixes

Land on `next`, take a candidate, validate it, then fast-forward. Shipping a
fix straight to `main` skips the only evidence that it works.

## Checking a release by hand

```bash
make verify-chart-images

docker buildx imagetools inspect \
  ghcr.io/thunder-compute/thunder-device-plugin/operator:<tag>
```
