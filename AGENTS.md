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

## Issue and PR Guidelines

- Never create an issue.
- Never create a PR.
- Never create or reply to comments.
