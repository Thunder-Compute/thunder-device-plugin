# Releasing

Thunder Compute runs this chart in its own cloud before anyone else gets it.
That is the whole point of the branch layout: a release is not a build, it is a
**promotion** of something that already ran in production.

```
next  ──●──●──●──●─────────  candidates: 0.2.0-rc.1, -rc.2, -rc.3 …
         └── main ──────●     fast-forwarded onto a proven candidate, tagged v0.2.0
```

## The two branches

| Branch | What it means |
| --- | --- |
| `next` | Integration. Every commit publishes a release candidate. |
| `main` | What customers get. Only ever fast-forwarded onto a candidate that has already run in Thunder's cloud. |

Every pull request targets `next`. Nothing is ever pushed to `main` directly —
not a hotfix, not a docs typo. Because `main` only fast-forwards, the two
branches can never diverge, so there is no cherry-picking and no merge to get
wrong: the commit customers run is the identical commit Thunder ran.

## Versions

`charts/thunder-device-plugin/Chart.yaml` carries the version being **cooked**,
not the last one released. Bump it on `next` immediately after a promotion.

Candidates are numbered by distance from `main`, so the count restarts at each
promotion. `hack/release-version.sh` is the single source of that arithmetic;
CI never computes a version any other way.

```bash
make release-version     # 0.2.0-rc.3
```

The chart leaves `operator.image.tag` and `daemon.image.tag` empty, so images
follow `appVersion`. Pinning a chart version therefore pins everything — there
is no second knob to keep in sync.

## The loop

1. **Merge to `next`.** CI runs `make check` and the kind cluster test. On
   green, `release-candidate.yaml` builds and signs the images, publishes the
   chart to `oci://ghcr.io/thunder-compute/charts`, and cuts a prerelease
   `v0.2.0-rc.N`.

2. **Bake in staging.** In thundernetes, set `chartRevision` for the
   `thunder-device-plugin` app in `infra/manifest/values-staging.yaml` to the
   new tag.

3. **Bake in production.** Move the same tag into `infra/manifest/values.yaml`
   and let it flow through the usual `main` → `giga-prod` promotion.

4. **Fast-forward `main`.** Once the candidate has run in production long
   enough to trust:

   ```bash
   git push origin v0.2.0-rc.3^{commit}:main
   ```

   This fails if it is not a fast-forward, which is exactly the protection
   wanted.

5. **Release.** Run the `release` workflow from `main` with the candidate
   version as input. It re-tags the candidate's image manifests — no rebuild —
   publishes the chart at the release version, verifies the digests match, and
   cuts `v0.2.0`.

6. **Flip the production pin** in thundernetes from `v0.2.0-rc.3` to `v0.2.0`.
   **This sync must be a no-op.** Same digests, so nothing but the image tag
   strings move. If Argo shows anything more, the released artifact is not the
   one that baked, and the release is wrong.

7. **Bump `Chart.yaml`** on `next` to the next version.

## Hotfixes

Land on `next`, take a candidate, give it a short bake, then fast-forward.
Shipping a fix straight to `main` skips the only evidence that it works.

## Checking a release by hand

```bash
make verify-promotion CANDIDATE=0.2.0-rc.3 RELEASE_VERSION=0.2.0

cosign verify ghcr.io/thunder-compute/thunder-dra-operator:0.2.0 \
  --certificate-identity-regexp '^https://github.com/Thunder-Compute/thunder-device-plugin/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
