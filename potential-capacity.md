# Potential Capacity

---

## Goals

- Establish the requirements for a **potential capacity API** — the mechanism by which a
  capacity provider communicates its catalog of launchable configurations to the
  scheduler, analogous to the information Karpenter's provisioner obtains today from its
  internal `GetInstanceTypes` API.
- Define the **offering** as the atomic unit of that catalog and the properties it must
  carry.
- Explore the challenges of offering **cardinality** and the constraints that arise from
  moving this catalog onto the API server, and motivate a **transformer model** as the
  direction for keeping it tractable.

## Non-goals

- **Explore alternate communication mechanisms in depth.** A plugin interface or a direct
  HTTP / gRPC endpoint are viable ways to communicate potential capacity. This doc treats
  a CRD-based approach as the direction to explore and only sketches those alternatives as
  a fallback; a thorough evaluation is out of scope.
- **Specify the concrete offering and transformation schema.** The API layout here is a
  conceptual model. The concrete CRD schema — including predicate and merge semantics — is
  deferred to a follow-up (see [Next Steps](#next-steps)).
- **Close the NodeClaim and DRA representation gaps.** The per-offering granularity gap in
  the NodeClaim API and the representation of DRA resources are known limitations, each
  deferred to its own follow-up (see [Next Steps](#next-steps)).

---

## Offerings — The Atomic Unit

### Prelude

A capacity provider exposes some number of **instance types**, each with a set of
requirements — well-known labels like zone, capacity type, architecture, OS, and
provider-specific labels like instance family or generation. The total configuration
space an instance type can be launched into is the cartesian product of these
requirements: an n-dimensional matrix where each dimension is a requirement key and
each cell is a concrete combination of values.

Crucially, this matrix is **sparse**. Not every cell corresponds to a launchable
configuration — an instance type may be available on-demand in one zone but not
another, may be offered as spot in only a subset of zones, or may be reservable only
where a matching capacity reservation exists. The valid cells are a subset of the
full product.

**Why this matters.** Provisioning is largely a set-constraint problem. The
requirements of a prospective node are the intersection of the requirements of the
source NodePool and the requirements of every pod assigned to it. Each pod placement
narrows that set further. If a placement narrows the compatible set to *empty* — no
cell in the matrix remains launchable — then that placement **must** be rejected: we
would otherwise commit to a node the capacity provider cannot actually launch. To
reason about this correctly, the scheduler needs to know which cells of the matrix
represent a real, launchable configuration.

Each such cell is an **offering**. An offering carries a set of requirements that
identify a unique coordinate in the n-dimensional matrix, along with the metadata the
scheduler needs to select between offerings and model their scarcity. The set of
available offerings is precisely the information provisioning is missing when it only
knows an instance type's aggregate requirements.

> Karpenter models this today with the internal `GetInstanceTypes(NodePool)` API,
> which returns `InstanceType`s each carrying a slice of `Offering`s. The upstream
> potential-capacity API will intentionally diverge from these types — the field
> layout below is a conceptual model, not a 1:1 mapping onto Karpenter's structs.

### Properties

An offering is defined by four properties.

- **Topology.** The set of labels that serve as the offering's coordinates within the
  configuration space. These are what make an offering addressable and what the
  scheduler intersects against pod and NodePool requirements. Common examples are
  zone, capacity type (spot / on-demand / reserved), and — for reserved capacity — a
  reservation identifier. The topology labels are the offering's identity: two
  offerings for the same instance type must differ in at least one topology label.

- **Weight.** A relative ordering factor used to break ties between candidate
  offerings when more than one can satisfy a request. It answers "all else being
  equal, which offering should we launch first?" Weight **defaults to cost**, which is
  the natural ordering for most capacity providers, but the property is intentionally
  abstract. Because it is just an ordering factor, customers can bias it to express a
  *preferred pool* — for example, skewing the effective cost of certain offerings to
  reflect price-to-performance rather than raw dollars, so the scheduler prefers
  instance types that deliver more useful work per dollar for their workloads.

- **Capacity.** The total number of nodes that can be launched for this offering.
  This models pools with *finite* capacity — most importantly capacity backed by
  on-demand capacity reservations, where only a fixed number of instances of a given
  type and zone are available. An unbounded offering (ordinary on-demand / spot)
  reports no limit; a bounded one lets the scheduler avoid over-committing to a pool
  it will exhaust.

- **Resources.** The Kubernetes resources this offering will make available to pods.
  It is tempting to treat resources as a property of the *instance type* rather than
  the offering, but that is insufficient — see below.

#### Why resources belong on the offering

The same instance type can, under different launch configurations, present different
resources to Kubernetes. Attaching resources to the offering (the concrete launchable
configuration) rather than the instance type (the aggregate) is what lets the
scheduler reason about these cases correctly. Three motivating examples:

1. **VM memory overhead.** The base capacity a node reports (`status.capacity`) is not
   deterministic from the instance type alone. On EC2, the hypervisor and the machine
   image consume an arbitrary portion of the advertised memory, so the *actual*
   allocatable memory depends on the instance type **and** the AMI. It is deterministic
   for a given (instance type, AMI) pair, but not across AMIs — which means the same
   instance type launched under two NodeClasses with different AMIs is genuinely two
   different offerings from a resource standpoint.

2. **Mutually exclusive resources.** A single instance type may advertise different
   resources depending on which drivers are active. A forward-looking example we are
   considering for enabling DRA in EKS Auto Mode: for a given piece of hardware we may
   ship *two distinct drivers* in the AMI — a DRA driver and its device-plugin
   equivalent — and only one may be active on a node at a time. Karpenter selects which
   driver to run dynamically, passing it as a launch parameter determined by the chosen
   offering. Because the two drivers advertise resources differently and are mutually
   exclusive, each combination is a distinct offering — and with `n` such drivers, each
   independently selectable (nothing forces all of them into DRA or all into
   device-plugin mode), the offering set multiplies by up to `2^n`.

3. **Implicit, request-driven configurations.** We are actively building a feature
   (not yet publicly described) where enabling it publishes an extended resource and
   simultaneously reduces the node's available memory. It must be enabled *implicitly*
   from pod requests rather than from static NodeClass configuration — which again is
   most naturally expressed as an alternate offering for the same instance type: one
   with the feature (extended resource present, memory reduced) and one without.

In all three cases the resources cannot be hoisted up to the instance type without
losing information, and they cannot be inferred by the scheduler without provider
knowledge. Encoding them at the offering level keeps the offering as the single,
self-describing atomic unit of launchable capacity.

### Sources of cardinality

The offering is the right atomic unit, but its expressiveness comes at a cost: the
number of offerings grows multiplicatively with every dimension a capacity provider
chooses to expose. It is worth cataloguing where that growth comes from, because it
directly motivates the [transformer model](#the-transformer-model) introduced later.

#### Highest contributors

- **Offering availability** (varies by provider — e.g., instance type × zone, or per
  capacity reservation). Capacity providers do not have infinite capacity for every
  instance type. When a provider cannot satisfy a request due to insufficient
  capacity, it returns an error, and the provisioner needs to know which
  configurations are currently unavailable so it can fall back to alternatives.
  Availability is tracked per offering, so it is a primary driver of cardinality.

- **VM memory overhead** (instance type × machine image). As described above, base
  memory varies by (instance type, AMI). Since the AMI can vary per NodeClass, this
  minimally multiplies the offering count by the number of unique machine-image
  configurations in the cluster.

#### Other contributors

- **Capacity reservations.** Each NodeClass can select a set of capacity reservations
  to make available. On EC2 a reservation is specific to an instance type and zone, so
  in practice this duplicates every offering matching that (instance type, zone,
  NodeClass) coordinate. The same reservation can be selected by multiple NodeClasses,
  compounding the effect.

- **Placement groups.** An EC2 construct letting users colocate instances (e.g., on
  the same spine). From the offering's perspective they behave like capacity
  reservations — each adds a duplicated, coordinate-specific offering.

- **Kubelet configurations.** Customers can vary kubelet settings (system-reserved,
  kube-reserved, etc.) per NodeClass. Because these change the allocatable resources
  for the same instance type, the same instance type under different NodeClasses
  yields distinct offerings.

- **Mutually exclusive resources.** As covered above (mutually exclusive DRA and
  device-plugin drivers, and the implicit request-driven feature), a single instance
  type may fan out into multiple offerings that differ only in advertised resources —
  up to `2^n` for `n` independently selectable drivers.

- **…and so on.** This list is not exhaustive; it extends arbitrarily as capacity
  providers and Kubernetes expose new features at the offering level. Each such
  feature is, in the worst case, another multiplicative factor on the offering count.

---

## Cardinality

The individual contributors above compound. To see the scale a real cluster can reach,
consider a deliberately conservative example.

A customer runs a cluster in `us-east-1`, a region with 1354 unique instance types (at time of writing).
Their cluster has subnets in 3 zones and uses 2 capacity types (spot + on-demand). They
run 5 NodeClasses, each with a distinct AMI or kubelet configuration, so allocatable
resources differ per NodeClass. The flat offering count is:

```
1354 instance types × 3 zones × 2 capacity types × 5 NodeClasses = 40,620 offerings
```

At a conservative ~2 KiB per offering, that is roughly **80 MiB** of offering data —
about **4%** of ETCD's default storage quota. And
this is the *conservative* case: the count grows rapidly in clusters with more
NodeClasses or more complex configurations (capacity reservations, placement groups),
and every new feature exposed at the offering level is potentially another
multiplicative factor — recall that a single family of mutually-exclusive drivers alone
can multiply the set by `2^n`.

Representing the fully-materialized, flat offering set is not impossible, but it poses a
near-term scalability problem: it is expensive to store, expensive to keep current
(each offering is an object the API server must serve and watchers must receive), and it
degrades as clusters and provider features grow.

### Dealing with cardinality

There are two broad strategies for keeping the potential-capacity API tractable.

**Communicate out-of-band (a plugin, or a direct HTTP / gRPC endpoint).** Rather than
materializing offerings as API objects, the capacity provider could serve them to the
scheduler through a plugin interface or a network endpoint. We do not explore this
in depth here (see [Non-goals](#non-goals)), but it carries three main drawbacks: reduced
observability (offerings are no longer inspectable as first-class cluster state),
packaging and distribution challenges, and higher integration friction for third-party
components. **This doc treats a CRD-based approach as the direction to explore; the
out-of-band options are a fallback should a CRD-based approach prove infeasible.**

**Model offerings as a base set plus transformations.** This is the direction this doc
pursues, developed below.

### The transformer model

The key observation is that most of the changes that inflate cardinality are *not*
arbitrary — they can be expressed as **transformations that apply to a well-defined
subset of the existing offerings**:

- On-demand capacity reservations create a duplicate of each offering matching a
  coordinate on (instance type, zone, NodeClass).
- Kubelet configurations apply the *same* modification to every offering belonging to a
  NodeClass.
- VM memory overhead applies to every offering matching a coordinate on (instance type,
  machine image) — and even in clusters with many NodeClasses, relatively few distinct
  machine images are typically in use, so the overhead can be deduplicated across them.

If those patterns can be expressed declaratively, the capacity provider need not ship
the fully-materialized set. Instead it ships a **compact base set of offerings**
plus an **ordered list of transformations**, and the consumer resolves the full set by
folding the transformations over the base. The materialized set exists only transiently,
in the scheduler's memory; what lives on the API server is the much smaller
base-plus-transforms representation.

#### How a transformation works

Each transformation is a **predicate** identifying the subset of offerings it applies to,
paired with one or more **outputs**. Each output is a partial offering that is
deep-merged onto a matched offering: fields the output sets are written, fields it leaves
unset are inherited from the source. This yields three fundamental behaviors:

- **Fan-out** — one matched offering produces several. A NodeClass selects a set of
  subnets, and each subnet exposes one zone to the NodeClass. A transform matches the
  offerings belonging to that NodeClass and expands each into one offering per exposed
  zone, with every other field passing through unchanged:

  ```
  # predicate: offerings for the NodeClass whose subnets expose 2a/2b/2c
  # outputs: one partial per exposed zone — only the zone label is written
  match:   offering.requirements["karpenter.k8s.aws/ec2nodeclass"] == "default"
  outputs:
    - { requirements: { "topology.kubernetes.io/zone": "us-east-1a" } }
    - { requirements: { "topology.kubernetes.io/zone": "us-east-1b" } }
    - { requirements: { "topology.kubernetes.io/zone": "us-east-1c" } }
  ```

- **Duplicate-and-mutate** — the source is kept and an additional, modified variant is
  emitted alongside it. A capacity reservation is only available on offerings matching a
  specific (instance type, zone, NodeClass) coordinate, so the transform matches that
  coordinate and adds a reserved variant alongside each matched offering:

  ```
  match:   offering.requirements["node.kubernetes.io/instance-type"]  == "m5.large"   &&
           offering.requirements["topology.kubernetes.io/zone"]        == "us-east-1a" &&
           offering.requirements["karpenter.k8s.aws/ec2nodeclass"]     == "default"
  outputs:
    - {}                                              # identity: keep on-demand
    - { requirements: {                               # add reserved variant
          "karpenter.sh/capacity-type": "reserved",
          "karpenter.sh/capacity-reservation-id": "cr-abc123" } }
  ```

- **Passthrough** — offerings not matched by a transformation's predicate flow through
  untouched.

View an interactive visualization of this transformation pipeline [here](https://potential-capacity-api-hld.netlify.app/).

> The predicate and merge syntax above is **illustrative only** — it conveys the
> mechanics, not the committed API. The concrete offering and transformation schema is
> deferred to a follow-up (see [Next Steps](#next-steps)).

#### Transformations compose as a DAG

Transformations are applied in order, and a later transformation can match offerings
*produced* by an earlier one. In the examples above, the reservation transform reads the
zone label that the fan-out transform wrote — so it must run after it. The result is a
directed acyclic graph: the base set flows through the ordered transforms, each stage
producing a complete resolved offering set, and the counts fold as it goes (one base
offering fans to three zones, one of which fans again into on-demand + reserved: 1 → 3 →
4). Because each stage is itself a complete, well-defined offering set, the model is
straightforward to reason about and to validate.

#### An important constraint

The transformer model **must not reduce the expressiveness of the API**. Anything
representable as a flat offering set must remain representable as a base set plus
transformations; the model is a compression of the same information, not a restriction
of it. For brevity, and because the schema is still iterating, the concrete API is
excluded from this document — but preserving full expressiveness is a hard requirement of
the design, and the current prototype meets it.

---

## Limitations

The transformer model is the direction this doc advances, but it is not free. This
section names the costs it introduces relative to the status quo — an in-process
`GetInstanceTypes` call that returns a fully-materialized, atomic snapshot.

### Complexity

Moving from a flat set to a base-plus-transforms representation shifts work onto both
sides of the API.

On the **producer** (capacity provider) side, the offering set no longer serializes
directly — the provider must decide *which* transformations most effectively factor its
offering set, since the reduction depends on choosing transforms that align with the
regular structure of its offerings (per-NodeClass kubelet configs, per-coordinate
reservations, per-machine-image overhead). This is a modeling burden that the flat
representation does not impose.

On the **consumer** (scheduler) side, resolving the offering set now requires folding an
ordered list of transforms over the base rather than reading a ready-made slice. This
cost can be largely absorbed by a **shared library** that implements the fold once, so
that individual consumers and third-party integrators are not each reimplementing the
resolution logic.

### Atomicity

Karpenter's `GetInstanceTypes(NodePool)` API returns offerings as a single, atomic
snapshot: the scheduler never observes a partially-updated set. Splitting the
representation into a base set and separate transformation objects gives that up. When
updates span multiple objects, the scheduler can resolve an offering set from a mix of
old and new state — a torn read. Two consequences are worth calling out.

**Relative ordering (weight).** The scheduler orders candidate offerings by weight
(cost, by default) and prefers the cheapest. If a torn read applies a more expensive
offering before its cheaper counterpart is visible, the scheduler may launch the more
expensive option. With an appropriate customer configuration this is eventually corrected
by consolidation, but only after incurring unnecessary node churn and cost in the
interim. This can be mitigated by requiring clients to publish offering updates in
**ascending order of weight**, so a cheaper offering is never hidden behind a
not-yet-visible one — but doing so converts what could be a guarantee of the API into an
implementation detail every client must honor correctly.

**Ordered preferences.** The ascending-weight mitigation does not help with constraints
that express an *explicit* ordering rather than a weight-based one — e.g. "prefer
instance type A, then fall back to B." A torn read can cause the scheduler to skip the
preferred option and land on a lower-preference one, and because this ordering is not a
function of weight, publishing in weight order does not prevent it. This too can only be
corrected during consolidation. It is worth noting rather than a dealbreaker: the
mechanism for expressing these preferences (preferred node affinity) is already
best-effort, so occasionally landing on a lower-preference option is within its existing
contract. This is made more relevant by the proposed waterfall preferences API that's been discussed for SDA.

---

## Next Steps

This doc establishes the offering as the atomic unit of potential capacity and the
transformer model as the direction for keeping its cardinality tractable. Several
threads are deliberately left for follow-up work.

### DRA representation

Each DRA driver publishes a pool of resources for a node as one or more `ResourceSlice`
objects. A pool may span multiple objects because a single object can exceed the ETCD
size limit for the number of potential resources it needs to describe. As a result, we
cannot embed a node's full DRA resources directly into an offering or transformation
object. A follow-up will discuss how DRA resources can be represented within the
potential-capacity API and what options exist to minimize their impact — whether on ETCD
for a CRD-based approach, or on networking and memory for a networked or plugin approach.

### Updated NodeClaim schema

Karpenter's internal potential-capacity representation already supports the capabilities
described in this doc, but its NodeClaim API has not expanded to match. The primary gap
is the lack of per-offering granularity: a NodeClaim expresses its compatible offerings
through a single set of requirements, which can only capture offerings that share a
common requirements intersection. Compatible offerings belonging to **disjoint** sets —
those with no shared intersection to express them jointly — cannot be represented. A
follow-up doc will address this limitation.

### Offering + transformer schema

For brevity and because the design is still iterating, this doc intentionally omits the
concrete API. A follow-up will present the proposed offering and transformation schema for
a CRD-based communication mechanism, including the predicate and merge semantics sketched
illustratively above. It will additionally include experimental data demonstrating cardinality reduction in real scenarios.
