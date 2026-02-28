# Creating a New Karpenter Provider

A step-by-step guide to getting started with a new Karpenter cloud provider implementation.

---

## 1. Understand the Karpenter Architecture

- Review the [Karpenter core repository](https://github.com/kubernetes-sigs/karpenter) to understand the separation between **core logic** (scheduling, deprovisioning, drift) and **cloud provider–specific logic**.
- Study an existing provider (e.g., [karpenter-provider-aws](https://github.com/aws/karpenter-provider-aws)) as a reference implementation.

## 2. Scaffold the Provider Repository

- Create a new Go module (e.g., `karpenter-provider-<cloud>`).
- Add `sigs.k8s.io/karpenter` as a dependency to pull in the core library.
- Set up your project structure following the convention:
  ```
  karpenter-provider-<cloud>/
  ├── cmd/controller/        # Main entrypoint
  ├── pkg/
  │   ├── apis/              # CRD types (NodeClass)
  │   ├── providers/         # Cloud API integrations
  │   ├── cloudprovider/     # CloudProvider interface impl
  │   └── operator/          # Operator/controller setup
  ├── charts/                # Helm chart
  ├── hack/                  # Code-gen & utility scripts
  └── test/                  # Integration/e2e tests
  ```

## 3. Define Your Custom Resource (NodeClass)

- Create a CRD type (e.g., `MyCloudNodeClass`) that captures cloud-specific configuration (images, networking, storage, instance profiles, etc.).
- Use `controller-gen` to generate the CRD manifests and deep-copy methods.
- Register the types with the scheme in your `apis/` package.

## 4. Implement the `CloudProvider` Interface

- Implement the `cloudprovider.CloudProvider` interface from Karpenter core. The key methods are:
  - **`Create(ctx, nodeClaim)`** — Launch a new instance in your cloud.
  - **`Delete(ctx, nodeClaim)`** — Terminate an instance.
  - **`Get(ctx, providerID)`** — Retrieve the current state of an instance.
  - **`List(ctx)`** — List all instances managed by Karpenter.
  - **`GetInstanceTypes(ctx, nodePool)`** — Return available instance types with their capabilities (CPU, memory, GPU, architecture, etc.).
  - **`IsDrifted(ctx, nodeClaim)`** — Determine if a node has drifted from its desired spec.

## 5. Build Cloud API Integration Providers

- Create internal provider packages for each cloud API you need to interact with:
  - **Instance provider** — Create, describe, delete VMs/instances.
  - **Instance type provider** — Discover and cache instance type offerings and pricing.
  - **Image provider** — Resolve the correct machine image (AMI, image ID, etc.).
  - **Network/subnet provider** — Discover available subnets and availability zones.
  - **Security group / firewall provider** — Resolve security configurations.
- Use caching where appropriate to avoid excessive cloud API calls.

## 6. Wire Up the Controller / Operator

- In `cmd/controller/main.go`, initialize the Karpenter core operator and inject your `CloudProvider` implementation.
- Register any additional cloud-specific controllers (e.g., NodeClass status reconciliation, garbage collection of orphaned cloud resources).

## 7. Create the Helm Chart

- Build a Helm chart under `charts/` for deploying the controller.
- Include RBAC, ServiceAccount, Deployment, and CRD templates.
- Parameterize cloud-specific settings (credentials, region, endpoint, etc.).

## 8. Add Instance Type Data

- Build or generate a catalog of your cloud's instance types with their resource capacities.
- Map them to Karpenter's well-known labels (`node.kubernetes.io/instance-type`, `topology.kubernetes.io/zone`, `kubernetes.io/arch`, etc.).

## 9. Write Tests

- **Unit tests** — Test cloud provider logic with mocked cloud API clients.
- **Integration tests** — Validate CRD lifecycle and controller behavior against a real or emulated API.
- **E2E tests** — Deploy to a real cluster and verify end-to-end provisioning and deprovisioning.

## 10. Document and Release

- Add user-facing documentation for installing and configuring the provider.
- Set up CI/CD to build, test, and publish container images and Helm charts.
- Follow semantic versioning aligned with the Karpenter core version you depend on.

---

## Useful References

- Karpenter Core: https://github.com/kubernetes-sigs/karpenter
- AWS Provider (reference impl): https://github.com/aws/karpenter-provider-aws
- Karpenter Docs: https://karpenter.sh/docs/
