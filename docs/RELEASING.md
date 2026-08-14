# Releasing

`main` is the integration and release branch. Pull requests target `main`, and
every commit merged into it publishes both component images tagged with the
full Git commit SHA:

```text
main commit
    ├── ghcr.io/thunder-compute/thunder-device-plugin/daemon:<commit-sha>
    └── ghcr.io/thunder-compute/thunder-device-plugin/operator:<commit-sha>
```

The SHA-tagged images are immutable build artifacts. The release workflow
promotes those exact image manifests to a human-readable release version; it
does not compile or rebuild them.

## The workflow

1. Open a pull request against `main`. CI runs static checks, unit tests, chart
   rendering, and the kind cluster test.

2. Merge the pull request into `main`. `publish-images.yaml` builds and signs
   the daemon and operator from that exact commit, publishing both images under
   the commit SHA.

3. Test the SHA-tagged images and source commit as needed. The image tag lets
   an installation identify exactly which source commit it runs.

4. Run the `release` workflow from `main`, supplying a semantic version such as
   `0.2.0`. The workflow verifies that the selected `main` commit's images
   exist, retags their manifests as `0.2.0`, verifies the digests match, and
   publishes the Helm chart at version `0.2.0`.

The chart leaves `operator.image.tag` and `daemon.image.tag` empty, so packaged
`appVersion` supplies the release image tag. A chart release therefore points
at the images promoted by the same release workflow.

## Image references

The image repositories are shared by the Makefile, image workflow, chart, and
promotion verification:

```text
ghcr.io/thunder-compute/thunder-device-plugin/daemon:<tag>
ghcr.io/thunder-compute/thunder-device-plugin/operator:<tag>
```

The commit-SHA tags identify normal `main` builds. Release tags identify the
same manifests after promotion.

## Checking a release by hand

```bash
make verify-promotion \
  SOURCE_TAG=<main-commit-sha> \
  RELEASE_VERSION=0.2.0

cosign verify ghcr.io/thunder-compute/thunder-device-plugin/operator:0.2.0 \
  --certificate-identity-regexp '^https://github.com/Thunder-Compute/thunder-device-plugin/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Hotfixes

Open a pull request against `main`, let CI validate it, merge it, and wait for
the image publishing workflow to complete. Release that resulting `main`
commit through the normal workflow.
