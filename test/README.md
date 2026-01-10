# Karpenter Integration Testing

Karpenter leverages Github Actions to run our E2E test suites.

## Directories
- `./.github/workflows`: Workflow files run within this repository. Relevant files for E2E testing are prefixed with `e2e-`
- `./.github/actions/e2e`: Composite actions utilized by the E2E workflows
- `./test/suites`: Ginkgo suites (`regression`, `performance`)
- `./test/pkg`: Shared helpers used by suites
- `./test/pkg/environment`: Default NodePool/NodeClass fixtures and test flag wiring
- `./test/pkg/debug`: Debug collectors and object dumps used on failures
- `./hack`: Testing scripts

## Running locally (core repo)

From the repo root, after deploying the controller to your cluster:

```bash
TEST_SUITE=regression make e2etests
```

Filter with Ginkgo regexes:

```bash
FOCUS="DaemonSet" TEST_SUITE=regression make e2etests
SKIP="Disruption" TEST_SUITE=regression make e2etests
```

Valid suites are `regression` and `performance`.

# Integrating Karpenter Tests for Cloud Providers

This guide outlines how cloud providers can integrate Karpenter tests into their provider-specific implementations.

## Overview

Karpenter's testing framework is designed to be extensible across different cloud providers. By integrating these tests into your provider implementation, you can ensure compatibility with Karpenter's core functionality while validating your provider-specific features.

## Prerequisites

- A working Karpenter provider implementation
- Go development environment
- Access to your cloud provider's resources for testing
- Kubernetes cluster for integration testing

## Test Integration Steps

### 1. Set Up Test Infrastructure

Create a dedicated test directory in your provider repository:

```bash
mkdir -p test/suites
mkdir -p test/pkg/environment/yourprovider
```

### 2. Define Default NodePool and NodeClass

You need to define default NodePool and NodeClass configurations that will be passed to the Karpenter test framework. These configurations will be used during test runs to create nodes in your cloud provider.

Create the following files in your test directory:

**Default NodePool (`test/pkg/environment/yourprovider/default_nodepool.yaml`)**:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: Never
    budgets:
      - nodes: 100%
  limits:
    cpu: 1000
    memory: 1000Gi
  template:
    spec:
      expireAfter: Never
      requirements:
        - key: kubernetes.io/os
          operator: In
          values: ["linux"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
      nodeClassRef:
        group: karpenter.yourprovider.com
        kind: YourProviderNodeClass
        name: default
```

**Default NodeClass (`test/pkg/environment/yourprovider/default_providernodeclass.yaml`)**:

```yaml
apiVersion: karpenter.yourprovider.com/v1alpha1
kind: YourProviderNodeClass
metadata:
  name: default
spec:
  # Add your provider-specific configuration here
  # For example:
  # instanceType: t3.large
  # region: us-west-2
```

These files follow the same pattern that kubernetes-sigs/karpenter uses for its tests, as seen in the `test/pkg/environment/common` directory.

### 3. Import Karpenter Core Test Framework

Add the Karpenter core repository as a dependency in your Go module:

```bash
go get sigs.k8s.io/karpenter
```

### 4. Configure CI/CD Integration

Set up CI/CD pipelines to run your tests automatically. You can use the following Makefile target as a reference:

```makefile
upstream-e2etests: 
	CLUSTER_NAME=${CLUSTER_NAME} envsubst < $(shell pwd)/test/pkg/environment/yourprovider/default_providernodeclass.yaml > ${TMPFILE}
	go test \
		-count 1 \
		-timeout 3.25h \
		-v \
		$(KARPENTER_CORE_DIR)/test/suites/... \
		--ginkgo.focus="${FOCUS}" \
		--ginkgo.timeout=3h \
		--ginkgo.grace-period=5m \
		--ginkgo.vv \
		--default-nodeclass="$(TMPFILE)"\
		--default-nodepool="$(shell pwd)/test/pkg/environment/yourprovider/default_nodepool.yaml"
```

This command:
1. Substitutes environment variables in your NodeClass template
2. Runs the Karpenter core test suites with appropriate timeouts and parameters
3. Specifies your provider's default NodeClass and NodePool configurations

You can also set up GitHub Actions workflows:

```yaml
# .github/workflows/tests.yaml
name: Integration Tests
on:
  push:
    branches: [ main ]
  pull_request:
    branches: [ main ]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - name: Set up Go
        uses: actions/setup-go@v4
        with:
          go-version: '1.20'
      - name: Set up cloud provider credentials
        run: |
          # Set up your cloud provider credentials here
      - name: Run tests
        run: make upstream-e2etests
```

## Test Suites

The Karpenter core test framework includes the following test suites that will be executed against your provider implementation:

### Regression (`test/suites/regression`)

1. **Integration**
   - DaemonSet overhead with LimitRange defaults/defaultRequests for capacity accounting
   - Utilization and scheduling constraints: one-pod-per-node, do-not-disrupt, pod anti-affinity
   - NodePool validation rules for restricted labels and requirement operators/values
   - NodePool hash annotations: `nodepool-hash` and `nodepool-hash-version` propagation to NodeClaims
   - Repair policy behavior that ignores budgets, do-not-disrupt, and `terminationGracePeriodSeconds`
2. **NodeClaim**
   - Creation and spec propagation: labels, taints, resources, NodeClass references
   - Deletion and finalizer behavior for missing or NotReady NodeClass
   - Registration timeout cleanup and orphan instance GC
3. **Drift**
   - Budget windows for empty vs. non-empty drift
   - Replacement flow and `nodepool-hash-version` updates
   - Failure cases: replacement never registers/initializes or PDBs are unhealthy
4. **Termination**
   - Emptiness budgets and blocking windows
   - Pod drain ordering and `terminationGracePeriodSeconds` handling
   - Node and instance termination on delete
5. **Expiration**
   - Expiration timing and replacement scheduling for active workloads
6. **Chaos**
   - Runaway scale-up guardrails with consolidation and emptiness enabled
7. **StaticCapacity**
   - Static NodeClaim provisioning and NodePool spec propagation
   - Node limits, scale-down ordering, and do-not-disrupt handling
   - Drift flows and dynamic NodeClaim fallback
8. **Performance**
   - Simple and complex provisioning scenarios at scale
   - Simple and complex drift flows

### Performance (`test/suites/performance`)

1. **Basic deployment**
   - Two deployments with different resource profiles
2. **Provisioning**
   - Pending-pod provisioning and NodePool cost tracking
3. **Host name spreading**
   - Two deployments with hostname topology spread
4. **Host name spreading XL**
   - XL-scale hostname topology spread
5. **Wide deployments**
   - Thirty deployments with varied resources and topology constraints
6. **Do-not-disrupt**
   - Scale-out and consolidation with protected workloads
7. **Drift performance**
   - Drift replacement with topology constraints
8. **Interference**
   - Self anti-affinity interference scenarios

These test suites validate that your provider implementation correctly integrates with Karpenter's core functionality and can handle various operational scenarios.

## Example Providers

Reference these existing provider implementations for guidance:

- [AWS Provider](https://github.com/aws/karpenter-provider-aws)

## Getting Help

If you encounter issues while integrating Karpenter tests:

- Join the [#karpenter-dev](https://kubernetes.slack.com/archives/C04JW2J5J5P) channel in Kubernetes Slack
- Attend the bi-weekly working group meetings
- Open issues in the [Karpenter Core repository](https://github.com/aws/karpenter-core/issues)
