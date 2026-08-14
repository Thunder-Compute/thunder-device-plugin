# Security

## Reporting a vulnerability

Email **support@thundercompute.com**. Please do not open a public issue for
anything exploitable.

Include the chart version, the Kubernetes version, and enough detail to
reproduce. We will acknowledge within three business days.

## What is supported

Fixes go to the most recent release. There are no long-term support branches.

## Verifying what you install

Released chart values record the image tags that were published and verified
for that release. Resolve an image tag to its immutable manifest digest with:

```bash
docker buildx imagetools inspect \
  ghcr.io/thunder-compute/thunder-device-plugin/operator:<tag>
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
