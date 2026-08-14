# Contributing

## Where changes go

Pull requests target **`next`**. CI publishes images for changes merged there
and opens a reviewable chart-values PR before the result is promoted to
`main` — see [docs/RELEASING.md](docs/RELEASING.md).

## Before opening a pull request

```bash
make check       # gofmt, vet, unit tests, chart render and packaging checks
make test-local  # full install on a throwaway kind cluster; no GPU needed
```

CI runs both on every pull request.

## Chart changes

- Every value needs a `# --` doc comment in `values.yaml`, an entry in
  `values.schema.json`, and a default that is safe for a stranger's cluster.
  The schema rejects unknown keys, so a value that is not in it cannot be set.
- Never make the chart create the Thunder API token Secret. It takes the name
  of an existing one, so a token cannot end up in a release history.
- CI records the published source-commit image tag explicitly in `values.yaml`.
  Do not hand-edit these tags; merge the CI-generated values PR instead.
- Do not add anything that only makes sense in one particular cluster. Every
  deployment should be a plain values overlay on top of this chart, ours
  included. If a cluster needs something the chart cannot express, that is a
  missing chart value, not a reason to carry a private variant.

## Versioning

`Chart.yaml` carries the default chart version used for local renders. The
release workflow supplies the chosen release version when it packages the
chart. Promote only after the CI-generated image-values PR has merged.
