# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is this repo?

Karpenter is a Kubernetes node autoscaler. It watches for unschedulable pods, evaluates scheduling constraints, provisions nodes that meet pod requirements, and removes nodes when no longer needed. This repo (`sigs.k8s.io/karpenter`) is the cloud-provider-agnostic core; cloud-specific implementations (AWS, Azure, etc.) import it as a library.

## Build & Test Commands

```bash
make test                        # Run all unit tests (Ginkgo, with race detector)
make test FOCUS="test name"      # Run a specific test by name
make verify                      # Full verification: codegen, linting, formatting, vet
make presubmit                   # Full pre-submit check (verify + test + licenses + vulncheck)
make deflake                     # Run tests repeatedly until failure (find flakes)
make vulncheck                   # Check for known vulnerabilities

# Run a single package's tests directly:
go test ./pkg/controllers/provisioning/... --ginkgo.focus="test description"

# E2E tests (requires a running cluster with kwok provider):
make e2etests TEST_SUITE=regression FOCUS="test name"
```

## Code Generation

Run `go generate ./...` to regenerate CRDs, deepcopy methods, and other generated code. The `make verify` target does this automatically and will fail CI if generated files are stale. After running generation, apply the perl fix for controller-tools parameterized types:
```bash
perl -i -pe 's/sets.Set/sets.Set[string]/g' pkg/scheduling/zz_generated.deepcopy.go
```

Tools are managed via `go.tools.mod` and invoked as `go tool -modfile=go.tools.mod <toolname>`.

## Architecture

### API Types (`pkg/apis/`)
- `v1/` — Stable APIs: `NodePool`, `NodeClaim` (the core abstractions)
- `v1alpha1/` — Alpha APIs: `NodeOverlay`
- `autoscaling/v1alpha1/` — `CapacityBuffer`
- CRDs are generated into `pkg/apis/crds/` and copied to `kwok/charts/crds/`

### CloudProvider Interface (`pkg/cloudprovider/types.go`)
The `CloudProvider` interface is what cloud-specific implementations must satisfy: `Create`, `Delete`, `Get`, `List`, `GetInstanceTypes`, `IsDrifted`, `RepairPolicies`, `Name`, `GetSupportedNodeClasses`. The `fake` subpackage provides a test double.

### Controllers (`pkg/controllers/`)
- **provisioning/** — Watches unschedulable pods, batches them, runs the scheduler, and creates NodeClaims
- **disruption/** — Handles consolidation (single-node, multi-node), drift, and emptiness
- **nodeclaim/** — Lifecycle management, consistency checks, garbage collection, expiration
- **node/** — Health monitoring and termination
- **nodepool/** — Counter, hash, readiness, validation controllers
- **state/** — The `Cluster` struct: an in-memory representation of cluster state (nodes, pods, instance types, daemon overhead). Many controllers depend on it.

### Scheduling (`pkg/scheduling/`)
The scheduling simulator: takes pending pods and available instance types, produces a set of NodeClaims. Handles requirements, topology spread, affinity/anti-affinity, volume limits, host ports.

### State (`pkg/state/`)
`Cluster` is the central in-memory cache of cluster state. Controllers inform it of changes; the provisioner and disruption controller read from it.

### Operator (`pkg/operator/`)
Bootstrap logic: creates the controller-runtime manager, registers schemes, injects options/context. Cloud providers call into this to build their operator binary.

### KWOK Provider (`kwok/`)
A lightweight fake cloud provider for local development and testing. It implements the `CloudProvider` interface using simulated instance types from `kwok/cloudprovider/instance_types.json`.

## Testing Patterns

- Tests use **Ginkgo/Gomega** with `envtest` (a real API server, no full cluster needed)
- Test setup lives in `pkg/test/` — `test.NewEnvironment()` starts envtest, `test.NodePool()`, `test.NodeClaim()` etc. are test fixtures
- `pkg/test/expectations/` has assertion helpers: `ExpectApplied`, `ExpectProvisioned`, `ExpectReconciled`, `ExpectDeleted`, `ExpectCleanedUp`
- Controller tests use a fake cloud provider (`pkg/cloudprovider/fake/`) and a fake clock (`k8s.io/utils/clock/testing`)
- Import conventions in test files: dot-import Ginkgo and Gomega, dot-import `pkg/test/expectations`

## Feature Gates

Controlled via `--feature-gates` flag or `FEATURE_GATES` env var. Current gates: `NodeRepair`, `ReservedCapacity`, `SpotToSpotConsolidation`, `NodeOverlay`, `StaticCapacity`, `CapacityBuffer`.

## Linting

Uses golangci-lint v2 (`.golangci.yaml`). Key settings:
- Max cyclomatic complexity: 11
- Required license header on all `.go` files (Apache 2.0, "Copyright The Kubernetes Authors")
- Import formatting: standard, third-party, then local module (`sigs.k8s.io/karpenter`)
- `kubeapilinter` runs only on `pkg/apis/`

## Module Layout

- `go.mod` — Main module (`sigs.k8s.io/karpenter`)
- `go.tools.mod` — Tool dependencies (controller-gen, ginkgo, golangci-lint, etc.), isolated from the main module
