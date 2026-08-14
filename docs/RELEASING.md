# Releasing

The release unit is the chart composition in Git. The component images are
published first, then the exact image tag is written into
`charts/thunder-device-plugin/values.yaml` before that change is committed and
pushed.

```text
make publish-images
    ├── ghcr.io/thunder-compute/thunder-device-plugin/daemon:<generated-tag>
    ├── ghcr.io/thunder-compute/thunder-device-plugin/operator:<generated-tag>
    └── values.yaml records <generated-tag> for both images
```

The image tag is a generated UTC build timestamp. It is an image identifier,
not a release-candidate or release-version suffix. The important pin is the
committed value in the chart.

## The workflow

1. Make the code change on `next` and run the local checks:

   ```bash
   make check
   ```

2. Publish the component images and update the chart values in one command:

   ```bash
   make publish-images
   ```

   To choose a tag explicitly, use `make publish-images IMAGE_TAG=20260814T153000Z`.
   The command builds and pushes both images with that same tag, then updates
   `operator.image.tag` and `daemon.image.tag` in `values.yaml`. Review that
   file, commit it with the code change, and push `next`.

3. The `release-candidate` workflow runs on `next`. It verifies that both
   image references recorded in `values.yaml` exist, packages the chart, and
   creates a prerelease. It does not build a second image or invent another
   tag.

4. Test the candidate chart. When it is ready, fast-forward `main` to the
   candidate commit.

5. Run the `release` workflow from `main`, supplying the candidate (for
   example, `0.2.0-rc.3`). It verifies that `main` is exactly that candidate,
   checks the same image references again, and publishes the chart using the
   version in `Chart.yaml`. It does not rebuild or retag component images.

6. After promotion, bump `Chart.yaml` on `next` for the next release.

## Image references

The image repositories are shared by the Makefile, chart, and workflows:

```text
ghcr.io/thunder-compute/thunder-device-plugin/daemon:<tag>
ghcr.io/thunder-compute/thunder-device-plugin/operator:<tag>
```

The chart's values file records the tag selected for each release. This is
needed because Thundernetes renders this chart directly from the pinned Git
commit; it does not infer image tags from a packaged OCI chart's `appVersion`.

## Checking a release by hand

```bash
make verify-chart-images
docker buildx imagetools inspect \
  ghcr.io/thunder-compute/thunder-device-plugin/operator:<tag>
```

## Hotfixes

Open a pull request against `next`, run `make publish-images` after the image
changes are ready, commit the resulting values update, and let the candidate
workflow validate it. Promote that candidate through `main` using the normal
workflow.
