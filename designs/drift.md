Karpenter Deprovisioning - Drift

Problem

Provisioners and AWSNodeTemplates (AWS) are declarative APIs that dictate the desired state of nodes. User’s requirements for their machines as reflected in these CRDs can change over time. For example, they can add or remove labels or taints from their nodes, modify their instance type requirements in the Provisioner, or change the Subnets discovered by their AWSNodeTemplate. To enforce that requirements set in their CRDs are applied to their fleet, users must manually terminate all out-of-spec nodes to rely on Karpenter to provision in-spec replacements.

Karpenter’s drift feature automates this process by automatically (1) detecting nodes that have drifted and (2) safely replacing the capacity.


(For more on why drift matters, Fairwinds wrote a great blog post about repercussions of unhandled drift in Kubernetes)

Recommended Solution

Karpenter automatically detects when nodes no longer match their corresponding specification in the Provisioner or AWSNodeTemplate. When this occurs, Karpenter triggers the standard deprovisioning workflow on the node. Karpenter Drift will not add API, it will only build on top of existing APIs. More on this decision below in 🔑 API Choices.

Karpenter Drift classifies each CRD field as a (1) Static, (2) Dynamic, or (3) Behavioral field and will evaluate them differently. Each of the currently defined fields in Karpenter CRDs will be classified as follows:

For Static Fields, values in the CRDs are reflected in the node in the same way that they’re set. A node will be detected as drifted if the values in the CRDs do not match the values in the node. Yet, since some external controllers directly edit the nodes, this is expanded to be more flexible below in 🔑 Static Field Flexibility.

Dynamic Fields can correspond to multiple values and must be handled differently. Dynamic fields can create cases where drift occurs without changes to CRDs, or where CRD changes do not result in drift.

For example, if a node has node.kubernetes.io/instance-type: m5.large, and requirements change from node.kubernetes.io/instance-type In [m5.large] to node.kubernetes.io/instance-type In [m5.large, m5.2xlarge], the node will not be drifted because it's value is still compatible with the new requirements. Conversely, if a node is using ami: ami-abc, but a new AMI is published, Karpenter's amiSelector will discover that the new correct value is ami: ami-xyz, and detect the node as drifted.

Behavioral Fields are used as over-arching settings on the Provisioner to dictate how Karpenter behaves. These fields don’t correspond to settings on the node and instance. They’re set by the user to control Karpenter’s Provisioning and Deprovisioning logic. Since these don’t map to a desired state of nodes, these fields will not be considered for Drift.

	Static	Dynamic	Behavioral
--- Provisioner Fields ---	---------------------	--------------------------	----------------------
Startup Taints	x
Taints	x
Labels	x
Annotations	x
Node Requirements		x
Kubelet Configuration	x
Weight			x
Limits			x
Consolidation			x
TTLSecondsUntilExpired + TTLSecondsAfterEmpty			x
--- AWSNodeTemplate Fields ---	---------------------	--------------------------	----------------------
Subnet Selector		x
Security Group Selector		x
Instance Profile	x
AMI Family + AMI Selector		x
UserData	x
Tags	x
Metadata Options	x
Block Device Mappings	x
Detailed Monitoring	x

Design Questions

🔑 API Choices

Drift is an existing mechanism in Karpenter and relies on standard Provisioning CRDs in Karpenter - Provisioners and AWSNodeTemplates. Drift is toggled through a feature gate in the karpenter-global-settings ConfigMap.

Once Drift is implemented as described above, the Drift feature gate will be removed, and Drift will be enabled by default. There will be no API additions.

In the future, users may want Karpenter to not Drift certain fields. This promotes users to not rely on CRDs as declarative sources of truth, breaking the contract that Drift defines. If a user request to opt out a field from Drift is validated, Karpenter should first rely on other Deprovisioning control mechanisms to enable this. Otherwise, Karpenter could add a setting in the karpenter-global-settings ConfigMap to handle it.

🔑 Static Field Flexibility

Problem

Users that run other controllers in their clusters that edit settings of the nodes directly like Cilium and NVIDIA GPU Operator may find Drift too unstable for Static Fields, since Karpenter will drift nodes that don’t exactly match the CRDs. If a user has an external controller that edit labels on the node, or edit tags on the instance, Karpenter will drift those nodes any time this external controller makes changes. Karpenter should work in tandem with external systems that users run in their clusters.

Solution

Karpenter could only consider Static Drift on nodes when the respective CRDs change. External systems and users can edit the nodes and instances as they please, as long as they don’t edit Dynamic Fields. This allows external systems to modify nodes and instances, but keep the drift automation of rolling out changes to settings to their fleet.

As a reference, this is how Kubernetes Deployments work. A user configures a PodTemplateSpec in the DeploymentSpec to reflect a desired state of the pods it manages. A user can edit a pod’s metadata without invoking Deployment drift. Deployment drift will only occur if a user edits the Deployment’s PodTemplateSpec.

🔑 In-place Drift

Some node settings that are generated from the fields in the Provisioner and AWSNodeTemplate can be drifted in-place. For example, Kubernetes supports editing the metadata of nodes (e.g. labels, annotations) without having to delete and recreate it. If a user adds a new distinct key-value pair to their Provisioner labels, Karpenter could add the label in-place, bypassing the entire deprovisioning flow. The same follows if a user adds a new distinct key-value pair to their AWSNodeTemplate tags. Yet, in-place tagging may fail as there are tag limits on EC2 instances.

Since each CRD field holds different use-cases, and the standard deprovisioning flow is to replace nodes that have drifted, Any special cases of in-place drift mechanism will need to be separately designed and implemented as special deprovisioning logic. For initial design and implementation, Drift will only replace nodes, and in-place drift can be designed in the future.


Notes

* static drift is 1 way, dynamic drift is 2 ways.
* value add is the ability to refresh the fleet when the Provisioner changes. Doing 2 way drift on static values is hard and trigger happy.
* Add Examples of Reasonable Static Drift
