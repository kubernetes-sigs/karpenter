# Agent Development Guide

A file for [guiding coding agents](https://agents.md/).

## Commands

- **Build:** `go build ./...`
  - `make build` builds the KWOK controller image with `ko` — only needed for
    deploying to a cluster, not for checking compilation.
- **Test:** `make test`
  - Tests are Ginkgo suites; the full run is slow. Prefer targeting a package:
    `go test ./pkg/controllers/disruption/... -race`
  - Focus a single spec: `FOCUS="<spec text>" make test` or pass
    `--ginkgo.focus="<spec text>"` to `go test`.
- **Lint/codegen:** `make verify`
  - Runs `go generate`, CRD validation scripts, `go vet`, and golangci-lint.
    Fails if it leaves a diff, so run it after changing APIs in `pkg/apis`.
- **Everything (pre-commit):** `make presubmit`
- **E2E:** `TEST_SUITE=<suite> make e2etests` (needs a cluster with the KWOK
  provider installed: `make install-kwok apply`)

## Directory Structure

- CRD APIs (NodePool, NodeClaim): `pkg/apis/`
- Core controllers (provisioning, disruption, etc.): `pkg/controllers/`
- Cloud provider interface + fake: `pkg/cloudprovider/`
- Scheduling library: `pkg/scheduling/`, cluster state: `pkg/state/`
- Test fixtures and expectation helpers: `pkg/test/`
- KWOK reference provider (local dev): `kwok/`
- E2E suites: `test/suites/`

## Controllers

- Every controller (a type implementing `controller.Controller`, i.e. with a
  `Register(ctx, manager) error` method and registered in
  `pkg/controllers/controllers.go`) exposes its name via:

  ```go
  func (c *Controller) Name() string { return "<literal>" }
  ```

  Use a plain string literal — not a `Sprintf`, a concatenation, or a struct
  field. The literal must match the name passed to `.Named(...)` during
  registration. Controller names are used only for the `controller` dimension
  on metrics and for logging, so a literal is sufficient and keeps the full set
  of names scrapeable directly from source.

## Metric Labels

Metric dimensions and their values are documented in code as
`metrics.Label{Name, Help, Values}` / `metrics.Value{Name, Help}`; the provider's
docs generator turns them into per-dimension help and value tables.

- Describe every dimension with a `Label`; never pass a bare string literal as a
  metric's label name.
- Reuse an existing `Label` when one already fits; reference its `.Name` rather
  than redeclaring the dimension.
- Define a `Label` in the most generic package that fits — shared dimensions in
  `pkg/metrics`, operator-agnostic ones upstream in operatorpkg — and only
  co-locate it in its owning package when an import cycle prevents centralizing.
- Enumerate a dimension's stable values as `metrics.Value`s, listing the
  well-known ones even when the set is not exhaustive. A value's `Name` comes from
  a const, never a magic string; a value that exists only as a metric value should
  be a first-class `metrics.Value` var that its emission site references by `.Name`.

## Issue and PR Guidelines

- Never create an issue.
- Never create a PR.
- Never create or reply to comments.

## Error Handling

- Use `serrors` everywhere, and only use standard structured keys already
  established in the repository.
- For Kubernetes resources, use Kind keys such as `Node`, `NodeClaim`, and
  `NodePool` with `klog.KObj` or `klog.KRef`.
