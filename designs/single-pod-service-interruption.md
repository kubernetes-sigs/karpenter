

## Motivation

When a deployment has only one pod, and this pod is running on node1 (the node created by karpenter), when node1 is terminated (upcoming interruption events or consolidation). From the time the pod are start evict to the time the new pod are created and run successfully on the new node, the deployment has no running pods to provide services.
This also happens when all pods of the Deployment are running on this node.

According to best practices, each deployment should have at least 2 pods. but in non-production environments, such as test environments and pre-release environments, which are usually functional verifications. Most services are single-copy. When R&D is testing, a node outage occurs and the corresponding service of the pod above becomes unavailable. If the service interruption time can be minimized when the node is terminated, the R&D experience will be better.

issues [#1674](https://github.com/kubernetes-sigs/karpenter/issues/1674)

pull request [#1685](https://github.com/kubernetes-sigs/karpenter/pull/1685)


## Solutions


### Option 1: kubernetes itself supports `surge eviction` feature

Upstream kubernetes supports `surge eviction` feature. When a deployment has only one pod, when it starts to evict the pod on the node, kubernetes will first create a new pod, and the old pod will not be terminated until the new pod runs successfully.

Pros
- 👍 This is the best solution. The karpenter component does not need any modification to adapt to the above problems.

Cons
- 👎 There is no definite time for upstream to support this feature. Currently, people who encounter this problem have no good way to solve it, which causes some troubles for non-production environments that are using karpenter.


### Option 2: The external controller handles this scenario

An additional controller needs to be run in the cluster. When the node starts to terminate, if it detects that there is only one pod deployment on the node, use pod-disruption-budget and have Karpenter just create an annotation on the node for that case: "I want to disrupt this node but I can't", then stop terminate this node

That would allow an external controller to do:
- Taint the node
- Find deployments responsible for the node pods
- Restart such deployments
  They would not be scheduled to the same node, if the new pods are Ready, the old ones get deleted, PDB is respected and Karpenter would be able to disrupt node the next check.

Pros
- 👍 The amount of karpenter code modification is relatively small
- 👍 No need to add update permissions to deployment in karpenter

Cons
- 👎 Need to develop and deploy additional controllers in the cluster, need to handle the linkage between the two controllers
- 👎 If the controller restarts the deployment, it will cause additional replicaset to increase. For services that use deployment rollback, it will cause the previous version to be rolled back unexpectedly.


### Option 3: The karpenter component itself supports this feature

karpenter develops a new feature to replace pod eviction with deployment restart (when the Deployment has only one pod)
When the node starts to terminate, a judgment will be made here. If all replicas of the Deployment are on this node, or the Deployment has only one pod, restarting the Deployment is more elegant than evicting. This operation will first create a pod on the new node, wait for the new pod to run successfully, and then terminate the old pod, which will reduce service interruption time.

Built the above restart deployment operation into the karpenter component, As a feature provided to users for selection, it is turned off by default. When the feature is turned on, the helm chart template adds the permission to restart the deployment according to the switch of this feature.

Pros
- 👍 This can solve the service interruption problem when the node is terminated when the deployment has only one pod. No need to deploy additional controllers in the cluster.
- 👍 Only when the user needs this feature, you can turn it on, and the helm template will take effect deployment restart permission

Cons
- 👎 If the controller restarts the deployment, it will cause additional replicaset to increase. For services that use deployment rollback, it will cause the previous version to be rolled back unexpectedly.
- 👎 When the restart deployment instead of eviction feature is enabled, additional deployment restart permissions are required.


## Recommendation

- There is no clear time point for option 1, so the karpenter component is recommended to implement this function.
- The external controller method requires the deployment of an additional controller and increases the coordination work between the two controllers.
- Therefore, it is recommended that the karpenter component build it into a feature to handle this problem.








