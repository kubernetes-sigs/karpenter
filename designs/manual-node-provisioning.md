# RFC: Manual node provisioning with Overprovisioner API
## Background
The reason for this is mainly the cost and performance tradeoffs. the overprovisioning with low priority pods would work but that would be too much costly. For example, there is a live event for which a faster sustainable scale up is needed, however the spike in traffic would be for some time and post that normal autoscaling could handle. This could be similar in case of Online shopping sales where a spike is there for some time and post that normal autoscaling could handle. To sustain this spike, keeping all the pods in running state all the time would be too much costly. If it is done manually before such an event, then it would be operationally burdensome. To solve this a hybrid approach could be used where, there might be 1% of nodes in running state and 20% nodes rest in stopped state to scale up faster. The better part is, since the stopped instance can be modified to any needed instance type, the ice erros can be handled. In my tests, I'm getting about a minute of time saving to launch an instance and it seems a better option to have in karpenter, unless the cloud provider provides faster startup time.

## Abstract
This document proposes the introduction of a new, dedicated Custom Resource Definition (CRD), Overprovisioner, to help with manual node provisioining in Karpenter. This new API is designed to exist alongside the NodePool CRD, separating the concern of defining node characteristics from the concern of managing spare capacity.

The Overprovisioner CRD will enable following things
- help maintain a buffer of pre-warmed nodes in various states(Running, Stopped, or Hibernated) to balance cost against pod scheduling latency. 
- It will support multiple sizing strategies, including fixed counts and percentages of total cluster size
- It will introduce distinct operational modes like KeepMinimum (for a static resource floor) and KeepEmpty (for dynamic, proportional buffering).  

This approach provides a Kubernetes-native toolkit to address faster scale up support while maintaining the cost-performance tradeoff.

## Motivation

While Karpenter excels at "just-in-time" reactive scaling, a significant class of workloads demands faster node availability than even its optimized provisioning can provide. The core motivation remains the elimination of the "cold start" latency inherent in launching new virtual machines. However, user stories and operational realities reveal a need for a more nuanced approach than a single "static capacity" switch.

Different scenarios require different trade-offs between cost, speed, and complexity:  
- **Maximum Performance (Running state):** For latency-critical applications like single-replica databases or CI/CD job runners, having a fully Running node ready to accept pods in seconds is paramount. The cost of an idle running node is secondary to minimizing downtime or developer wait time.

- **Balanced Cost and Speed (Hibernated state):** Many applications can tolerate a slightly longer startup time if it results in significant cost savings. Hibernated instances, which preserve their memory state on disk, offer a compelling middle ground. They resume much faster than a cold boot but incur minimal costs while paused (paying only for storage). This is ideal for development environments or applications with predictable but not instantaneous scaling needs.

- **Maximum Cost Savings (Stopped state):** For workloads that are less time-sensitive but still benefit from pre-provisioned capacity, Stopped instances offer the lowest cost, as only their EBS volumes are billed. While startup is slower than from hibernation, it is still faster than provisioning from scratch. 

Furthermore, the strategy for maintaining this buffer capacity varies. Some users need a constant, guaranteed number of spare nodes, while others prefer a dynamic buffer that scales proportionally with the cluster's total size. Attempting to shoehorn this rich set of requirements into the existing NodePool API would lead to an overly complex and bloated interface. A dedicated Overprovisioner API provides the necessary clarity and expressive power to manage these advanced strategies effectively.

## Proposal: The Overprovisioner CRD
We propose the creation of a new Custom Resource Definition (CRD) named Overprovisioner. This CRD will target an existing NodePool to define the type of capacity to be overprovisioned, while the Overprovisioner resource itself will define the quantity, state, and strategy for that spare capacity.
### API DefinitionYAML
```apiVersion: karpenter.sh/v1beta1
kind: Overprovisioner
metadata:
  name: my-app-overprovisioner
spec:
  # A reference to the NodePools that defines the characteristics 
  # (instance types, taints, architecture, etc.) of the nodes to overprovision.
  targetNodePools: 
    - amd-workloads
    - arm-workloads
    - gpu-workloads

  # The desired state for the overprovisioned nodes.
  # Options: Running, Stopped, Hibernated.
  # Default: Running
  state: Running

  # The strategy for managing the overprovisioned capacity.
  # KeepEmpty(If nodepool of 5 nodes gets consumed by 2 nodes then it will spin up another 2 nodes and make overprovisioned nodes as 5).
  # KeepMinimum(if nodepool of 5 nodes gets consumed by 2 nodes then it will keep 3 nodes only as overprovisioned)
  # Default: KeepEmpty
  strategy: KeepEmpty

  # Defines the quantity of overprovisioned nodes.
  # Only one of count or percentage can be specified.
  sizing:
    # A static number of nodes to keep in the specified state.
    count: 2 
    # A percentage of the total number of nodes managed by the targetNodePool.
    # For example, a value of 10 would maintain a 10% buffer.
    percentage: null

  # defines overrides for schedueld time
  scheduledOverrides:
    - schedule: "0 9 * * mon-fri"
      duration: 8h
      # Only one of count or percentage can be specified.
      sizing:
        count: 2
        percentage: null
```
### Controller Behavior
A new overprovisioner controller will be introduced into Karpenter. This controller will:
1. Watch for Overprovisioner resources.
2. For each Overprovisioner, it will monitor the associated targetNodePool and the nodes currently managed by it.
3. Based on the specified strategy and sizing, it will calculate the target number of spare nodes.
4. It will compare the current number of spare nodes in the desired state with the target number.
5. If there is a deficit, it will request that the Karpenter provisioner launch new nodes with a special label (e.g., `karpenter.sh/capacity-type: overprovisioned`) and manage them through the appropriate lifecycle for the specified state.
6. If there is a surplus, it will terminate spare nodes to match the desired count.  

Nodes labeled as `karpenter.sh/capacity-type: overprovisioned` will be exempt from standard consolidation logic.

### Node Lifecycle Management
The state field dictates how the overprovisioner controller manages the node lifecycle:
- **state: Running:** This is the "hot pool" option. The controller provisions a node that fully joins the cluster. It will be tainted (e.g., `karpenter.sh/overprovisioned: "true":NoSchedule`) to prevent immediate pod scheduling. When a new pod needs a node, this taint can be removed, and the pod can be scheduled in seconds.
- **state: Stopped:** This is a "cold pool" option for cost savings. The controller provisions a node, allows it to initialize, and then issues a StopInstances API call to the cloud provider. The node does not join the Kubernetes cluster. When capacity is needed, the controller issues a StartInstances call. This is slower than Running but significantly cheaper.
- **state: Hibernated:** This is the "warm pool" option. The process is similar to Stopped, but the controller issues a HibernateInstances call. This saves the instance's RAM to the root EBS volume, allowing for much faster resumes.

This offers a balance between the speed of Running and the cost of Stopped, though it has more cloud-provider-specific prerequisites. When a pod needs capacity from a Stopped or Hibernated pool, the overprovisioner controller will start the instance, wait for it to join the cluster, and then make it available for scheduling.


### Strategies Explained
The strategy field provides high-level control over the behavior of the overprovisioning logic.

#### KeepMinimum Strategy: 
This strategy ensures that a minimum number of spare nodes are always available, regardless of the cluster's overall utilization. It is ideal for:  
    - **Predictable Bursts:** Pre-scaling for known events where immediate capacity is non-negotiable.

When using KeepMinimum, the sizing.count field is most appropriate, providing a deterministic floor of spare capacity.


#### KeepEmpty Strategy (Proportional Overprovisioning): 
This strategy is designed to maintain a buffer of spare capacity that is proportional to the current size of the cluster, but it allows this buffer to scale down to zero if the cluster becomes completely empty. It is ideal for:  
    - **Handling Bursts:** Providing headroom for stateless applications to scale horizontally without waiting for new nodes.  
    - **Cost Efficiency:** Avoiding the cost of maintaining a buffer during periods of no activity. The buffer grows and shrinks with the cluster's usage.  

When using KeepEmpty, the sizing.percentage field is most appropriate, as it creates a dynamic buffer that adapts to the cluster's scale.  
The controller will use `ceil()` to round up, ensuring at least one spare node is created for any non-zero percentage in a running cluster.

## Alternatives Considered

One of the alternatives suggested was to extend the NodePool API directly[1]. This was rejected because it conflates two distinct responsibilities: defining the properties of a pool of capacity (NodePool) and defining the proactive management strategy for that capacity (Overprovisioner). A separate CRD provides a cleaner separation of concerns, a more intuitive user experience, and avoids creating an overly complex, monolithic API object. The common "pause pod" workaround was also considered and rejected as it is an operationally burdensome hack and not cost effective solve for larger spike.

### Possible implementation using CRDs

Here is the CRD mentioned which includes the overprovisioning in the existing definition. In this case, if there is a same overprovisioning config required for all the nodepools, the same config needs to be copied and managed as its part of the same CRD. This makes it a bit complex to manage them as well as it would be more error prone if the overprovisioning gets more complex with diff type of node state, stratagy, sizing and schedules. However, there is some upside in terms of some shared core functionality could be used, however, future enhancement could be hampered if this adds any other requirements.  
```apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: arm-nodepool
spec:
  disruption:
    budgets:
    - nodes: 5%
      reasons:
      - Underutilized
    - nodes: 25%
      reasons:
      - Empty
    - nodes: 1%
      reasons:
      - Drifted
    consolidateAfter: 2m0s
    consolidationPolicy: WhenEmptyOrUnderutilized
  limits:
    cpu: 15000
    memory: 30000Gi

  # Overprovisioning definition in the same CRD. the fields represents same usage as mentioend fpr separate CRD.
  overprovision:
  - state: Running
    stratagy: KeepMinimum
    sizing:
      count: 10
    scheduledOverrides:
    - schedule: "0 9 * * mon-fri"
      duration: 8h
      sizing:
        count: "0"
  - state: Stopped
    stratagy: KeepEmpty
    sizing:
      percentage: 20
  template:
    metadata:
      labels:
        availability-zone-id: az1
        nodegroup: graviton-nodes-common-workloads
        role: graviton-nodes-common-workloads
    spec:
      expireAfter: Never
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: graviton-workloads
      requirements:
      - key: node.kubernetes.io/instance-type
        operator: In
        values:
        - c7g.8xlarge
        - c7g.12xlarge
        - c7g.16xlarge
      - key: topology.kubernetes.io/zone
        operator: In
        values:
        - ap-south-1a
      - key: karpenter.sh/capacity-type
        operator: In
        values:
        - on-demand
      taints:
      - effect: NoSchedule
        key: nodegroup
        value: arm-nodes-common-workloads
  weight: 100
```

## Refs: 
1. Autoscaler Buffers Proposal - https://docs.google.com/document/d/1bcct-luMPP51YAeUhVuFV7MIXud5wqHsVBDah9WuKeo/edit?tab=t.0 
2. Manual Node Provisioning Thread - https://github.com/kubernetes-sigs/karpenter/issues/749  
3. Static capacity RFC - https://github.com/kubernetes-sigs/karpenter/pull/2309  
4. Search and Doc generation with help of Gemini AI


