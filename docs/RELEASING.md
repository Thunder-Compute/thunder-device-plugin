# Releasing

Image publication is owned by GitHub Actions. Developers do not need GHCR
push credentials and do not update release image tags by hand.

## The workflow

1. Open pull requests against `next`. CI runs static checks, unit tests, chart
   rendering, and the kind cluster test.

2. Merge the code change into `next`. `publish-images.yaml` builds and signs
   both component images using the GitHub Actions package token:

   ```text
   ghcr.io/thunder-compute/thunder-device-plugin/daemon:<source-commit-sha>
   ghcr.io/thunder-compute/thunder-device-plugin/operator:<source-commit-sha>
   ```

3. The workflow opens a bot PR that records that same source-commit SHA in
   both image tags in `charts/thunder-device-plugin/values.yaml`. Review and
   merge this PR into `next`. The chart commit now explicitly selects the
   images CI published.

4. Test the chart from the resulting `next` commit. Then merge `next` into
   `main`. Do not promote `main` before the image-values PR has merged.

5. Run the `release` workflow from `main`, supplying a semantic version such
   as `0.2.0`. The workflow verifies the images selected by `values.yaml`,
   packages the chart, and publishes the release. It does not rebuild or
   retag a different image.

Thundernetes renders this chart directly from its pinned Git commit, so the
explicit values update is required. Packaged chart `appVersion` metadata alone
is not sufficient for that deployment path.

## Image references

The image repositories are shared by CI, the chart, and the release workflow:

```text
ghcr.io/thunder-compute/thunder-device-plugin/daemon:<tag>
ghcr.io/thunder-compute/thunder-device-plugin/operator:<tag>
```

The source-commit tags are immutable build identifiers. Release chart values
continue to point at those exact tags.

## Checking a release by hand

```bash
make verify-chart-images
docker buildx imagetools inspect \
  ghcr.io/thunder-compute/thunder-device-plugin/operator:<tag>
```

## Hotfixes

Open the hotfix PR against `next`. After merging, wait for the CI image-values
PR, merge it, and then promote the combined result to `main` through the normal
workflow.
