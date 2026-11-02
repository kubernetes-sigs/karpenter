# NodeOverlay Max Capacity

## Background

A [NodeOverlay](./node-overlay.md) can add extended resources to the instance types it matches, with `spec.capacity`. The amount is fixed: every node launched from a matching instance type is assumed to have exactly that amount.

That model does not fit resources whose amount is decided when the node boots. Hugepages are the main example. The kernel reserves hugepages at boot, and kubelet reports the count when it starts. Today, to run a database that needs 8602Mi of 2Mi hugepages, a user must:

* create a NodePool and a NodeOverlay (and often a NodeClass) for each page count, and
* send each pod to the NodePool for its page count.

Each node then reserves the full page count for its pool, even when its pods need less. The number of NodePools grows with the number of page counts in use.

## Proposal

Add an optional field, `spec.maxCapacity`, to NodeOverlay. It sets a **ceiling** for an extended resource, not an exact amount. When Karpenter launches a node, it gives the node exactly what the pods packed onto it request, up to the ceiling. It tells the cloud provider the chosen amount with a NodeClaim annotation.

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: hugepages
spec:
  requirements:
    - key: karpenter.sh/nodepool
      operator: In
      values: ["postgres"]
  maxCapacity:
    hugepages-2Mi: 64Gi
```

With this overlay, a pod that requests `hugepages-2Mi: 8602Mi` gets a node with 8602Mi of 2Mi hugepages. A pod that requests none gets a node with none.

### API

* `maxCapacity` is a `ResourceList`. Like `capacity`, it must not contain `cpu`, `memory`, `ephemeral-storage` or `pods`.
* A resource must not be set in both `capacity` and `maxCapacity` of the same NodeOverlay. A CEL rule enforces this.
* Runtime validation also rejects:
  * values of zero or less
  * `hugepages-<size>` values that are not a whole number of pages
* The field is covered by the existing alpha `NodeOverlay` feature gate. When no NodeOverlay sets `maxCapacity`, Karpenter behaves exactly as before.

### Merging overlays

The merge rules for `capacity` are applied to both fields, per resource name:

* For each resource, the highest weight overlay wins, whether it sets the resource in `capacity` or in `maxCapacity`. A higher weight `capacity` therefore makes a resource fixed again, and a higher weight `maxCapacity` makes it flexible.
* Two overlays with the same weight that set the same resource, in either field, are in conflict. The overlay that is evaluated second is marked with `ValidationSucceeded=False` and reason `Conflict`, as today.

### Scheduling

An instance type keeps the ceiling in its capacity, and records which resources are flexible.

* **Fit.** A request above the ceiling does not fit.
* **Hugepages and memory.** Kubernetes takes hugepages out of a node's allocatable memory. For a fixed amount, Karpenter already subtracts it from allocatable memory ahead of time. For a flexible amount, it cannot, because the amount is not known yet. Instead, a set of pods fits only if their memory requests plus their flexible hugepages requests fit in allocatable memory.
* **Chosen amount.** When scheduling finishes, the amount of each flexible resource is the pods' requests plus the largest daemonset overhead for that resource across the remaining instance types. Karpenter uses the largest overhead so the daemonsets fit whichever instance type launches. The NodeClaim's `spec.resources.requests` holds the same amount.
* **One amount per NodeClaim.** A NodeClaim can launch as any of several instance types, but it can carry only one amount. If a requested resource is flexible on some remaining instance types and fixed on others, Karpenter keeps only the flexible ones. It does this while it adds pods, so price filtering and truncation later on cannot break it. Resources that no pod requests are not narrowed.

### The annotation

Karpenter writes the chosen amounts on the NodeClaim:

```yaml
metadata:
  annotations:
    karpenter.sh/flexible-capacity: '{"hugepages-2Mi":"8602Mi"}'
```

* The value is a JSON map from resource name to quantity.
* It lists only flexible resources with an amount greater than zero.
* When the annotation is absent, the node gets none of the flexible resources. Nodes whose pods request no flexible resources look exactly as they did before this change.
* The annotation is internal: only Karpenter sets it.
* Karpenter copies NodeClaim annotations to the Node when the node registers, so the annotation is also on the Node.

### Cloud provider contract

A cloud provider that supports flexible capacity must, in `Create`:

1. read the annotation, with `cloudprovider.FlexibleCapacityFromNodeClaim`, and
2. give the node exactly those amounts, for example by setting `vm.nr_hugepages` in user data.

Karpenter itself sets the NodeClaim status. The NodeOverlay decorator wraps `Create`. When a NodeOverlay sets `maxCapacity` for the launched instance type, the decorator replaces each flexible resource in the returned `status.capacity` and `status.allocatable` with the chosen amount, or removes it when no amount was chosen. It also takes the chosen hugepages out of `status.allocatable.memory`. A provider can therefore report the instance type as it is, with the ceiling. The correction gives the same result if the provider already applied the amounts, for example with `cloudprovider.ApplyFlexibleCapacity`. The instance already exists when the decorator runs, so a failed correction is logged and does not fail the launch.

Kubernetes requires a hugepages request to be a whole number of pages, so the chosen amount is always a whole number of pages.

#### AWS

The AWS provider supports hugepages on the AL2 and AL2023 AMI families. AL2 user data also works on Ubuntu EKS AMIs.

* It adds a shell script to the user data. The script writes the page count for each page size to `/sys/kernel/mm/hugepages/hugepages-<size>kB/nr_hugepages`.
* The script runs after the user's own user data, so its page counts replace any that the user sets. It runs before kubelet starts:
  * on AL2, before `/etc/eks/bootstrap.sh`;
  * on AL2023, before nodeadm, which starts kubelet only after cloud-init has run the user data scripts.
* If the kernel allocates fewer pages than asked, the script frees cached memory, compacts memory and tries once more. It logs the result.
* User data scripts run only on the first boot. So the script also writes the 2Mi page count to `/etc/sysctl.d/zzzz-hugepages.conf`. On every later boot, systemd-sysctl sets `vm.nr_hugepages` from that file before kubelet starts. Other page sizes have no sysctl, so their counts are lost when the node reboots.
* The script runs after the user's own user data. If that user data also writes `/etc/sysctl.d/zzzz-hugepages.conf`, Karpenter's script replaces the file, so the node keeps Karpenter's count after a reboot too.
* The page counts are part of the user data, so each distinct page count gets its own launch template.
* On the other AMI families (Bottlerocket, Windows and Custom), a launch that needs hugepages fails with an error. Without this check, the node would start without its hugepages and never initialize.
* A flexible resource that is not hugepages also fails the launch, because the provider cannot allocate it.

Because the annotation is on the Node, a DaemonSet can also read it and allocate the pages when the node joins. This works on providers whose NodeClass cannot carry a page count. But kubelet reads the hugepages count only when it starts, and it sets the hugetlb limit of the `kubepods` cgroup at the same time. A DaemonSet must therefore restart kubelet after it allocates the pages. Allocation in user data, before kubelet starts, does not need a restart.

### After launch

* **In-flight and existing nodes.** Karpenter models a node from its NodeClaim status, and then from the Node. Both hold the chosen amount, so a launched node never takes pods beyond it.
* **Initialization.** A NodeClaim is not initialized until every resource it requests has non-zero allocatable on the Node. A node is therefore not treated as ready before its hugepages appear.
* **Consolidation.** Consolidation runs the same scheduling simulation, so a replacement node is sized to the pods that remain.
* **Drift.** Editing an overlay does not drift nodes. This is the same as `capacity` today.
* **NodePool limits.** While a NodeClaim is being built, its instance types are charged their full capacity against NodePool limits, including the ceiling. This only matters if a NodePool sets a limit on a flexible resource. After launch, the NodeClaim status holds the chosen amount.

## Known limits

* **Pages stay allocated.** The page count is fixed when the node boots. When a hugepages pod leaves:
  * a new pod that needs no more than the free pages can use them;
  * pods without hugepages can use only the memory left after the pages, because Linux keeps hugepages in a separate pool, and kubelet sets a hugetlb limit of zero for pods that do not request them;
  * consolidation frees the pages when a cheaper replacement fits the remaining pods. If the same instance type is still needed, the pages stay unused.

  For workloads that run one pod per node, a NodePool taint and `WhenEmpty` or `WhenEmptyOrUnderutilized` consolidation remove the node after its pod leaves. Workloads that run several pods per node may keep unused pages. Possible follow-ups are a metric for unused flexible capacity, or a consolidation rule for it.
* **Providers must opt in.** A provider that ignores the annotation launches a node without the resource. The NodeClaim requests the resource, so it never becomes initialized, and the pod stays pending. If `expireAfter` is `Never`, the node stays in this state until someone deletes it (kubernetes-sigs/karpenter#3242). A wrong fixed overlay has the same effect today.
* **Nodes cannot grow.** A later pod that needs more of a flexible resource than a node has gets a new node.

## Alternatives considered

* **Size the resource in the cloud provider.** The provider sees only the NodeClaim, after scheduling. Karpenter must know the amount during scheduling to decide which pods fit and which instance type to launch.
* **Per-offering capacity overrides.** `Offering.CapacityOverride` sets a different fixed amount for each offering. It does not let the amount follow the pods.
* **A separate feature gate.** NodeOverlay is already alpha and off by default, and `maxCapacity` does nothing unless it is set, so the existing gate is enough.

## Testing

* Unit tests cover validation, overlay merging and conflicts, instance type allocatable and fit, scheduling (the chosen amount, daemonset overhead, the ceiling, memory coupling, narrowing, in-flight nodes), consolidation sizing, and the KWOK provider.
* E2E tests in `test/suites/regression/nodeoverlay_test.go` run on kind with the KWOK provider. The KWOK provider reads the annotation and builds the node's capacity and allocatable from it. The tests show that:
  * fixed `capacity` overlays still work;
  * a node gets exactly the hugepages its pod requests, and its allocatable memory is reduced by that amount;
  * a request above the ceiling does not launch a node;
  * memory and hugepages together select a larger instance type;
  * nodes are sized to their own pods;
  * a pod without hugepages gets none;
  * a consolidation replacement is sized to the pods that remain;
  * a same-weight overlay that sets the resource in the other field is marked as a conflict.
