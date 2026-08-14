# Contributing

## Where changes go

Pull requests target **`next`**. Release candidates are tested there before
`main` is fast-forwarded to the selected candidate — see
[docs/RELEASING.md](docs/RELEASING.md).

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
- Published image tags are recorded explicitly in `values.yaml`. Run
  `make publish-images` after changing the images; it publishes both
  components with one generated tag and updates the chart before you commit.
- Do not add anything that only makes sense in one particular cluster. Every
  deployment should be a plain values overlay on top of this chart, ours
  included. If a cluster needs something the chart cannot express, that is a
  missing chart value, not a reason to carry a private variant.

## Versioning

`Chart.yaml` carries the chart version being prepared. Candidate versions are
derived from the commits since `main`; the release workflow packages the
version already present in `Chart.yaml` and does not change image tags.
