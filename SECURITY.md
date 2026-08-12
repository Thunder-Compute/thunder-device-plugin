# Security

## Reporting a vulnerability

Email **support@thundercompute.com**. Please do not open a public issue for
anything exploitable.

Include the chart version, the Kubernetes version, and enough detail to
reproduce. We will acknowledge within three business days.

## What is supported

Fixes go to the most recent release. There are no long-term support branches.

## Verifying what you install

Images and charts are signed with [cosign](https://docs.sigstore.dev/) from
GitHub Actions, keyless:

```bash
cosign verify ghcr.io/thunder-compute/thunder-dra-operator:<version> \
  --certificate-identity-regexp '^https://github.com/Thunder-Compute/thunder-device-plugin/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Notes on the components

- The **daemon runs privileged**. It enters host namespaces to run the Thunder
  installer and writes CDI specs the container runtime reads. It only schedules
  onto nodes labelled Thunder-eligible.
- The chart **never creates the Thunder API token Secret**. It takes the name of
  a Secret you create, so the token cannot reach a Helm release history.
- `libthunder.so` is downloaded once per node and verified against the digest
  pinned by the installer at `thunder.installURL` before it is staged into any
  container.
