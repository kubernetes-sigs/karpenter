# Karpenter v1 API

## Overview

In October 2023, the Karpenter maintainer team [released the beta version](https://aws.amazon.com/blogs/containers/karpenter-graduates-to-beta/) of the NodePool, NodeClaim, and EC2NodeClass APIs, promoting them on a trajectory toward stable (v1). After multiple months in beta, we have sufficient confidence in our APIs that we are ready to promote our APIs to stable support. This implies that we will not be changing these APIs in incompatible ways moving forward without a v2 version, which would still require us to provide long-term support for the launched v1 version.

This move to a stable version of our API represents our last opportunity to update our APIs in a way that we believe: sheds technical debt, improves the experience of all users, and offers additional extensibility for Karpenter’s APIs into future post-v1. This list of API changes below represents the minimal set of changes that are needed to ensure proper operational excellence, feature completeness, and stability by v1. For a change to make it on this list, it must meet one of the following criteria:

1. Breaking: The change requires changes or removals from the API that would be considered breaking after a bump to v1
2. Stability: The change ensures proper operational excellence for behavior that is leaky or has race conditions in the beta state
3. Planned Deprecations: The change cleans-up deprecations that were previously planned the project

## Migration Path

Karpenter will **not** be changing its API group or resource kind as part of the v1 API bump. By avoiding this, we can leverage the [existing Kubernetes conversion webhook process](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/#webhook-conversion) for upgrading APIs, which allows upgrades to newer versions of the API to occur in-place, without any customer intervention or node rolling. The upgrade process will be executed as follows:

1. Apply the updated NodePool, NodeClaim, and EC2NodeClass CRDs, which will contain a `v1` versions listed under the `versions` section of the CustomResourceDefinition
2. Upgrade Karpenter controller to its `v1` version. This version of Karpenter will start reasoning in terms of the `v1` API schema in its API requests. Resources will be converted from the v1beta1 to the v1 version at runtime, using conversion webhooks shipped by the upstream Karpenter project and the Cloud Providers (for NodeClass changes).
3. Users update their `v1beta1` manifests that they are applying through IaC or GitOps to use the new `v1` version.
4. Karpenter drops the `v1beta1` version from the `CustomResourceDefinition` and the conversion webhooks on the next minor version release of Karpenter, leaving no webhooks and only the `v1` version still present

## NodePool API

```
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    metadata:
      labels:
        billing-team: my-team
      annotations:
        example.com/owner: "my-team"
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws # Updated since only a single version will be served
        kind: EC2NodeClass
        name: default
      taints:
        - key: example.com/special-taint
          effect: NoSchedule
      startupTaints:
        - key: example.com/another-taint
          effect: NoSchedule
      requirements:
        - key: "karpenter.k8s.aws/instance-category"
          operator: In
          values: ["c", "m", "r"]
          minValues: 2 # Alpha field, added for spot best practices support
  disruption:
    budgets:
      - nodes: 0
        schedule: "0 10 * * mon-fri"
        duration: 16h
        reasons:
          - drifted
          - expired
      - nodes: 100%
        reasons:
          - empty
      - nodes: "10%"
      - nodes: 5
    expireAfter: 720h | Never
    consolidationPolicy: WhenUnderutilized | WhenEmpty
    consolidateAfter: 1m | Never # Added to allow additional control over consolidation aggressiveness
  weight: 10
  limits:
    cpu: "1000"
    memory: 1000Gi
status:
  conditions:
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: NodeClassReady
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Ready
  resources:
    cpu: "20"
    memory: "8192Mi"
    ephemeral-storage: "100Gi"
```

### Defaults

Defaults are unchanged in the v1 API version but are included in the API proposal for completeness.

`disruption.budgets` : `{"nodes": "10%"}`
`disruption.expireAfter`: `30d/720h`
`disruption.consolidationPolicy`: `WhenUnderutilized`

### Printer Columns

**Category:** Stability, Breaking

#### Current

```
➜  karpenter git:(main) ✗ kubectl get nodepools -o wide
NAME      NODECLASS   WEIGHT
default   default     100
fallback  fallback    
```

#### Proposed

```
➜  karpenter git:(main) ✗ kubectl get nodepools -o wide
NAME      NODECLASS  NODES  READY  AGE   WEIGHT  CPU%  MEMORY%
default   default    4      True   2d7h  100     5.8%  99%
fallback  default    100    True   2d7h          20%   20%
```

**Standard Columns**

1. Name
2. NodeClass - Allows users to easily see the NodeClass that the NodePool is using
3. Nodes - Allow users to easily view the number of nodes that are associated with a NodePool
4. Ready - NodePools now have status conditions and will only be used for scheduling when ready. This readiness should be easily viewable by users
5. Age - This is a common field for all Kubernetes objects and should be maintained in the standard set of columns

**Wide Columns (-o wide)**

1. Weight - Viewing the NodePools that will be evaluated first should be easily observable but may not be immediately useful to all users, particularly if the NodePools are named in a way that already indicate their ordering e.g. suffixed with fallback
2. CPU% - The capacity of the CPU for all nodes provisioned by this NodePool as a percentage of its limits
3. Memory% - The capacity of the memory for all nodes provisioned by this NodePool as a percentage of its limits

### Changes to the API

#### Moving `spec.template.spec.kubelet` to NodeClass

We are shifting the `spec.template.spec.kubelet` section from the NodePool and NodeClaim into the EC2NodeClass. This is called out in more detail in [Moving spec.template.spec.kubelet into the EC2NodeClass](#moving-spectemplatespeckubelet-into-the-nodeclass) which is covered in the EC2NodeClass section below.

#### Status Conditions

**Category:** Stability

Defining the complete set of status condition types that we will include on v1 launch is **out of scope** of this document and will be defined with more granularly in Karpenter’s Observability RFC. Minimally for v1, we will add a `NodeClassReady` and `Ready` condition so that we can determine whether a NodePool is ready to provision a new instance based on the NodeClass readiness provided by the Cloud Provider. More detail around why Karpenter needs status conditions for observability and operational excellence for node launches can be found in [#493](https://github.com/kubernetes-sigs/karpenter/issues/493) and [#909.](https://github.com/kubernetes-sigs/karpenter/issues/909)

Based on this, we are requiring that any Cloud Provider that wants to use this readiness mechanism to have a `status.conditions` section to their NodeClass which has a top-level “Ready” condition. If a Cloud Provider does not implement this API, no NodeClassReady status condition will be propagated onto the NodePool.

#### Status Condition Schema

**Category:** Breaking

Karpenter currently uses the [knative schema for status conditions](https://github.com/knative/pkg/blob/main/apis/condition_types.go#L58). This status condition schema contains a `severity` value which is not present in the upstream schema. Severity for status conditions is tough to reason about. Additionally, Karpenter is planning to [remove its references to knative as part of the v1 Roadmap](./v1-roadmap.md).

We should replace our status condition schema to adhere to the [upstream definition for status conditions](https://github.com/kubernetes/apimachinery/blob/master/pkg/apis/meta/v1/types.go#L1532). An example of the upstream status condition schema is shown below. The upstream API currently requires that all fields except the observedGeneration are required to be specified; we will deviate from this initially by only requiring that the `type`, `status`, and `lastTransitionTime` be set.

```
conditions:
  - type: Initialized
    status: "False"
    observedGeneration: 1
    lastTransitionTime: "2024-02-02T19:54:34Z"
    reason: NodeClaimNotLaunched
    message: "NodeClaim hasn't succeeded launch"
```

#### Add `consolidateAfter`

**Category:** Stability

The [Karpenter v1 Roadmap](./v1-roadmap.md) currently proposes the addition of the `consolidateAfter` field to the Karpenter NodePool API in the disruption block to allow users to be able to tune the aggressiveness of consolidation at v1 (e.g. how long a node has to not have churn against it for us to consider it for consolidation). See [#735](https://github.com/kubernetes-sigs/karpenter/issues/735) for more detail.

This feature is proposed to be added to the v1 API because it avoids us releasing a dead portion of the v1 API that is currently only settable when using `consolidationPolicy: WhenEmpty` and ensures that we are able to set a default for this field at v1 (since adding a default to would be a breaking change after v1).

#### Requiring all `nodeClassRef` fields

**Category:** Breaking

`nodeClassRef` was introduced into the API in v1beta1. v1beta1 did not require users to set the `apiVersion` and `kind` of the NodeClass that they were referencing. This was primarily because Karpenter is built as a single binary that supports a single Cloud Provider and each supported Cloud Provider right now (AWS, Azure, and Kwok) only support a singe NodeClass.

If a Cloud Provider introduces a second NodeClass to handle different types of node provisioning, it’s going to become necessary for the Cloud Provider to differentiate between its types. Additionally, being able to dynamically look up this CloudProvider-defined type from the neutral code is critical for the observability and operationalization of Karpenter (see [#909](https://github.com/kubernetes-sigs/karpenter/issues/909)).

#### Updating `spec.nodeClassRef.apiVersion` to `group`

**Category:** Breaking

APIVersions in Kubernetes have two parts: the group name and the version. Currently, Karpenter’s nodeClassRef is referencing the entire apiVersion for NodeClasses to determine which NodeClass should be used during a NodeClaim launch. This works fine, but is non-sensical given that the Karpenter controller will never use the **version** portion of the apiVersion to look-up the NodeClass at the apiserver; it will only ever use the group name on a single version.

Given this, we should update our `apiVersion` field in the `nodeClassRef` to be `group`. This updated field name also has parity with newer, existing APIs like the [Gateway API](https://gateway-api.sigs.k8s.io/api-types/gatewayclass/).

#### Removing `resource.requests` from NodePool Schema

**Category:** Stability

`spec.resource.requests` is part of the NodeClaim API and is set by the Karpenter controller when creating new NodeClaim resources from the scheduling loop. This field describes the required resource requests cacluated from the summation of pod resource requests needed to run against a new instance that we launch. This field is also used to establish Karpenter initialization, where we rely on the presence of requested resources in the newly created Node before we will allow the Karpenter disruption controller to disrupt the node.

We are currently templating every field from the NodeClaim API onto the NodePool `spec.template` (in the same way that a Deployment templates every field from Pod). Validation currently blocks users from setting resource requests in the NodePool, since the field does not make any sense in that context.

Since this field is currently dead API in the schema, we should drop this field from the NodePool at v1.

## NodeClaim API

```
apiVersion: karpenter.sh/v1
kind: NodeClaim
metadata:
  name: default
spec:
  nodeClassRef: 
    group: karpenter.k8s.aws # Updated since only a single version will be served
    kind: EC2NodeClass
    name: default
  taints:
    - key: example.com/special-taint
      effect: NoSchedule
  startupTaints:
    - key: example.com/another-taint
      effect: NoSchedule
  requirements:
    - key: "karpenter.k8s.aws/instance-category"
      operator: In
      values: ["c", "m", "r"]
      minValues: 2
  resources: 
    requests:
      cpu: "20"
      memory: "8192Mi"
status:
  allocatable:
    cpu: 1930m
    ephemeral-storage: 17Gi
    memory: 3055Mi
    pods: "29"
    vpc.amazonaws.com/pod-eni: "9"
  capacity:
    cpu: "2"
    ephemeral-storage: 20Gi
    memory: 3729Mi
    pods: "29"
    vpc.amazonaws.com/pod-eni: "9"
  conditions:
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Launched
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Registered
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Initialized
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Drifted
      reason: RequirementsDrifted
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Expired
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Empty
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Ready
  nodeName: ip-192-168-71-87.us-west-2.compute.internal
  providerID: aws:///us-west-2b/i-053c6b324e29d2275
  imageID: ami-0b1e393fbe12f411c
```

### Printer Columns

**Category:** Stability, Breaking

#### Current

```
➜  karpenter git:(main) ✗ kubectl get nodeclaims -o wide 
NAME            TYPE         ZONE         NODE                                            READY   AGE    CAPACITY   NODEPOOL   NODECLASS
default-7lh6k   c6gn.large   us-west-2b   ip-192-168-183-234.us-west-2.compute.internal   True    2d7h   spot       default    default
default-97v9h   c6gn.large   us-west-2b   ip-192-168-71-87.us-west-2.compute.internal     True    2d7h   spot       default    default
default-fhzpm   c7gd.large   us-west-2b   ip-192-168-165-122.us-west-2.compute.internal   True    2d7h   spot       default    default
default-rw4vf   c6gn.large   us-west-2b   ip-192-168-91-38.us-west-2.compute.internal     True    2d7h   spot       default    default
default-v5qfb   c7gd.large   us-west-2a   ip-192-168-58-94.us-west-2.compute.internal     True    2d7h   spot       default    default
```

#### Proposed

```
➜  karpenter git:(main) ✗ kubectl get nodeclaims -A -o wide 
NAME            TYPE         CAPACITY       ZONE         NODE                                            READY   AGE    ID                                      NODEPOOL  NODECLASS
default-7lh6k   c6gn.large   spot           us-west-2b   ip-192-168-183-234.us-west-2.compute.internal   True    2d7h   aws:///us-west-2b/i-053c6b324e29d2275   default   default
default-97v9h   c6gn.large   spot           us-west-2b   ip-192-168-71-87.us-west-2.compute.internal     True    2d7h   aws:///us-west-2a/i-053c6b324e29d2275   default   default
default-fhzpm   c7gd.large   spot           us-west-2b   ip-192-168-165-122.us-west-2.compute.internal   True    2d7h   aws:///us-west-2c/i-053c6b324e29d2275   default   default
default-rw4vf   c6gn.large   on-demand      us-west-2b   ip-192-168-91-38.us-west-2.compute.internal     True    2d7h   aws:///us-west-2a/i-053c6b324e29d2275   default   default
default-v5qfb   c7gd.large   spot           us-west-2a   ip-192-168-58-94.us-west-2.compute.internal     True    2d7h   aws:///us-west-2b/i-053c6b324e29d2275   default   default
```

**Standard Columns**

1. Name
2. Instance Type
3. Capacity - Moved from the wide output to the standard output
4. Zone
5. Node
6. Ready
7. Age

**Wide Columns (-o wide)**

1. ID - Proposing adding Provider ID to make copying and finding instances at the Cloud Provider easier
2. NodePool Name
3. NodeClass Name

### Changes to the API

#### NodeClaim Spec Immutability

**Category:** Breaking

Karpenter currently doesn’t enforce immutability on NodeClaims in v1beta1, though we implicitly assume that users should not be acting against these objects after creation, as the NodeClaim lifecycle controller won’t react to any change after the initial instance launch.

Karpenter can make every `spec` field immutable on the NodeClaim after its initial creation. This will be enforced through CEL validation, where you can perform a check like [`self == oldSelf`](https://kubernetes.io/docs/reference/using-api/cel/#language-overview)` to enforce that the fields cannot have changed after the initial apply. Users who are not on K8s 1.25+ that supports CEL will get the same validation enforced by validating webhooks.

## EC2NodeClass API [AWS CloudProvider]

```
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: default
spec:
  kubelet:
    podsPerCore: 2
    maxPods: 20
    systemReserved:
        cpu: 100m
        memory: 100Mi
        ephemeral-storage: 1Gi
    kubeReserved:
        cpu: 200m
        memory: 100Mi
        ephemeral-storage: 3Gi
    evictionHard:
        memory.available: 5%
        nodefs.available: 10%
        nodefs.inodesFree: 10%
    evictionSoft:
        memory.available: 500Mi
        nodefs.available: 15%
        nodefs.inodesFree: 15%
   evictionSoftGracePeriod:
       memory.available: 1m
       nodefs.available: 1m30s
       nodefs.inodesFree: 2m
   evictionMaxPodGracePeriod: 60
   imageGCHighThresholdPercent: 85
   imageGCLowThresholdPercent: 80
   cpuCFSQuota: true
   clusterDNS: ["10.0.1.100"]
  bootstrapMode: Bottlerocket
  subnetSelectorTerms:
    - tags:
        karpenter.sh/discovery: "${CLUSTER_NAME}"
    - id: subnet-09fa4a0a8f233a921
  securityGroupSelectorTerms:
    - tags:
        karpenter.sh/discovery: "${CLUSTER_NAME}" 
    - name: my-security-group
    - id: sg-063d7acfb4b06c82c
  amiSelectorTerms:
    - tags:
        karpenter.sh/discovery: "${CLUSTER_NAME}"
    - name: my-ami
    - id: ami-123
  role: "KarpenterNodeRole-${CLUSTER_NAME}"
  instanceProfile: "KarpenterNodeInstanceProfile-${CLUSTER_NAME}"
  userData: |
    echo "Hello world"
  tags:
    team: team-a
    app: team-a-app
  instanceStorePolicy: RAID0
  metadataOptions:
    httpEndpoint: enabled
    httpProtocolIPv6: disabled
    httpPutResponseHopLimit: 1 # This is changed to disable IMDS access from containers not on the host network
    httpTokens: required
  blockDeviceMappings:
    - deviceName: /dev/xvda
      ebs:
        volumeSize: 100Gi
        volumeType: gp3
        iops: 10000
        encrypted: true
        kmsKeyID: "1234abcd-12ab-34cd-56ef-1234567890ab"
        deleteOnTermination: true
        throughput: 125
        snapshotID: snap-0123456789
  detailedMonitoring: **true**
status:
  subnets:
    - id: subnet-0a462d98193ff9fac
      zone: us-east-2b
    - id: subnet-0322dfafd76a609b6
      zone: us-east-2c
    - id: subnet-0727ef01daf4ac9fe
      zone: us-east-2b
    - id: subnet-00c99aeafe2a70304
      zone: us-east-2a
    - id: subnet-023b232fd5eb0028e
      zone: us-east-2c
    - id: subnet-03941e7ad6afeaa72
      zone: us-east-2a
  securityGroups:
    - id: sg-041513b454818610b
      name: ClusterSharedNodeSecurityGroup
    - id: sg-0286715698b894bca
      name: ControlPlaneSecurityGroup-1AQ073TSAAPW
  amis:
    - id: ami-01234567890123456
      name: custom-ami-amd64
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values:
            - amd64
    - id: ami-01234567890123456
      name: custom-ami-arm64
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values:
            - arm64
  instanceProfile: "${CLUSTER_NAME}-0123456778901234567789"
  conditions:
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: InstanceProfileReady
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: SubnetsReady
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: SecurityGroupsReady
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: AMIsReady
    - lastTransitionTime: "2024-02-02T19:54:34Z"
      status: "True"
      type: Ready
```

### Printer Columns

**Category:** Stability, Breaking

#### Current

```
➜  karpenter git:(main) ✗ k get ec2nodeclasses -o wide 
NAME      AGE
default   2d8h
```

#### Proposed

```
➜  karpenter git:(main) ✗ k get ec2nodeclasses -o wide 
NAME     BOOTSTRAP  ROLE                            READY  AGE  
default  AL2        KarpenterNodeRole-test-cluster  True   2d8h   
```

**Standard Columns**

1. Name
2. Bootstrap - Bootstrap is a field that allows users to quickly categorize which bootstrap script mode their instances will be getting.
3. Ready - EC2NodeClasses now have status conditions that inform the user whether the EC2NodeClass has resolved all of its data and is “ready” to be used by a NodePool. This readiness should be easily viewable by users.
4. Age

**Wide Columns (-o wide)**

1. Role - As a best practice, we are recommending that users use a Node role and let Karpenter create a managed instance profile on behalf of the customer. We should easily expose this role.

#### Status Conditions

**Category:** Stability

Defining the complete set of status condition types that we will include on v1 launch is **out of scope** of this document and will be defined with more granularly in Karpenter’s Observability design. Minimally for v1, we will add a `Ready` condition so that we can determine whether a EC2NodeClass can be used by a NodePool during scheduling. More robustly, we will define status conditions that ensure that each required “concept” that’s needed for an instance launch is resolved e.g. InstanceProfile resolved, Subnet resolved, Security Groups resolved, etc.

#### Require AMISelectorTerms and Change `amiFamily` to `bootstrapMode`

**Category:** Stability, Breaking

When specifying AMIFamily with no AMISelectorTerms, users are currently configured to automatically update AMIs when a new version of the EKS-optimized image in that family is released. Existing nodes on older versions of the AMI will drift to the newer version to meet the desired state of the EC2NodeClass.

This works well in pre-prod environments where it’s nice to get auto-upgraded to the latest version for testing but is extremely risky in production environments. [Karpenter now recommends to users to pin AMIs in their production environments](https://karpenter.sh/docs/tasks/managing-amis/#option-1-manage-how-amis-are-tested-and-rolled-out:~:text=The%20safest%20way%2C%20and%20the%20one%20we%20recommend%2C%20for%20ensuring%20that%20a%20new%20AMI%20doesn%E2%80%99t%20break%20your%20workloads%20is%20to%20test%20it%20before%20putting%20it%20into%20production); however, it’s still possible to be caught by surprise today that Karpenter has this behavior when you deploy a EC2NodeClass and NodePool with an AMIFamily. Most notably, this is different from eksctl and MNG, where they will get the latest AMI when you first deploy the node group, but will pin it at the point that you add it.

We no longer want to deal with potential confusion around whether nodes will get rolled or not when using an AMIFamily with no `amiSelectorTerms`. Users will still be able to set `bootstrapMode` to prescribe userData that they want auto-injected by Karpenter, but it will no longer cause a default AMI to be selected. Instead, users will be required to set `amiSelectorTerms` to pin to an AMI.

#### Moving `spec.template.spec.kubelet` into the NodeClass

**Category:** Breaking

When the KubeletConfiguration was first introduced into the NodePool, the assumption was that the kubelet configuration is a common interface and that every Cloud Provider supports the same set of kubelet configuration fields.

This turned out not to be the case in reality. For instance, Cloud Providers like Azure [do not support configuring the kubelet configuration through the NodePool API](https://learn.microsoft.com/en-us/azure/aks/node-autoprovision?tabs=azure-cli#:~:text=Kubelet%20configuration%20through%20Node%20pool%20configuration%20is%20not%20supported). Kwok also has no need for the Kubelet API. Shifting these fields into the NodeClass API allows CloudProvider to pick on a case-by-case basis what kind of configuration they want to support through the Kubernetes API.

#### Disable IMDS Access from Containers by Default

**Category:** Stability, Breaking

The HTTPPutResponseHopLimit is [part of the instance metadata settings that are configured on the node on startup](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-options.html). This setting dictates how many hops a PUT request can take before it will be rejected by IMDS. For Kubernetes pods that live in another network namespace, this means that any pod that isn’t using `hostNetwork: true` [would need to have a HopLimit of 2 set in order to access IMDS](https://aws.amazon.com/about-aws/whats-new/2020/08/amazon-eks-supports-ec2-instance-metadata-service-v2/#:~:text=However%2C%20this%20limit%20is%20incompatible%20with%20containerized%20applications%20on%20Kubernetes%20that%20run%20in%20a%20separate%20network%20namespace%20from%20the%20instance). Opening up the node for pods to reach out to IMDS is an inherent security risk. If you are able to grab a token for IMDS, you can craft a request that gives the pod the same level of access as the instance profile which orchestrates the kubelet calls on the cluster.

We should constrain our pods to not have access to IMDS by default to not open up users to this security risk. This new default wouldn’t affect users who have already deployed EC2NodeClasses on their cluster. It would only affect new EC2NodeClasses.

## Labels/Annotations/Tags

#### karpenter.sh/do-not-consolidate (Kubernetes Annotation)

**Category:** Planned Deprecations, Breaking

`karpenter.sh/do-not-consolidate` annotation was introduced as a node-level control in alpha. This control was superseded by the `karpenter.sh/do-not-disrupt` annotation that disabled *all* disruption operations rather than just consolidation. The `karpenter.sh/do-not-consolidate` annotation was declared as deprecated throughout beta and is dropped in v1.

#### karpenter.sh/do-not-evict (Kubernetes Annotation)

**Category:** Planned Deprecations, Breaking

`karpenter.sh/do-not-evict` annotation was introduced as a pod-level control in alpha. This control was superseded by the `karpenter.sh/do-not-disrupt` annotation that disable disruption operations against the node where the pod is running on. The `karpenter.sh/do-not-evict` annotation was declared as deprecated throughout beta and is dropped in v1.

#### karpenter.sh/managed-by (EC2 Instance Tag) [AWS CloudProvider]

**Category:** Planned Deprecations, Breaking

Karpenter introduced the `karpenter.sh/managed-by` tag in v0.28.0 when migrating Karpenter over to NodeClaims (called Machines at the time). This migration was marked as “completed” when it tagged the instance in EC2 with the `karpenter.sh/managed-by` tag and stored the cluster name as the value. Since we have completed the NodeClaim migration, we no longer have a need for this tag; so, we can drop it.

This tag was only useful for scoping pod identity policies with ABAC, since it stored the cluster name in the value rather than `kubernetes.io/cluster/<cluser-name>` which stores the cluster name in the tag key. Session tags don’t work with tag keys, so we need some tag that we can recommend users to use to create pod identity policies with ABAC using OSS Karpenter.

Starting in v1, Karpenter would use `eks:eks-cluster-name: <cluster-name>` for tagging and scoping instances, volumes, primary ENIs, etc. and would use `eks:eks-cluster-arn: <cluster-arn>` for tagging and scoping instance profiles that it creates.
* * *

