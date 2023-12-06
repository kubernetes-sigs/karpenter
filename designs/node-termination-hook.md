# Custom Node termination (custom finalizer)

## Motivation
Users are interested in customizing how node terminations occurs and although the existing finalizer is very useful its difficult introduce any custom termination
logic for nodes [#743](https://github.com/kubernetes-sigs/karpenter/issues/743), [#622](https://github.com/kubernetes-sigs/karpenter/issues/622), [#740](https://github.com/kubernetes-sigs/karpenter/issues/740), [#690](https://github.com/kubernetes-sigs/karpenter/issues/690),
[#5232](https://github.com/aws/karpenter/issues/5232). The goal of this is to open up the finalizer such that users can provide their own controller but utilizing Karpenter's
existing logic for adding the finalizer to ensure the finalizer is created as soon as the NodeClaim + Node objects are created.

## Proposed Spec

```yaml
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: default
spec: # This is not a complete NodePool Spec.
  disruption:
    consolidationPolicy: WhenUnderutilized || WhenEmpty
    consolidateAfter: 10m || Never # metav1.Duration
    expireAfter: 10m || Never # Equivalent to v1alpha5 TTLSecondsUntilExpired
    customFinalizerName: example.com/customNodeTermination || nil
    terminationGracePeriodSeconds:  86400 || nil #https://github.com/kubernetes-sigs/karpenter/pull/834
```


## Implementation

### NodeClass 

https://github.com/kubernetes-sigs/karpenter/blob/3c16d48c27cf25338a6f1f634ffb30a1fd03d7d9/pkg/controllers/nodeclaim/lifecycle/controller.go#L87

Check for custom finalizer and writes that object instead of default finalizer.
```go
func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	// Add the finalizer immediately since we shouldn't launch if we don't yet have the finalizer.
	// Otherwise, we could leak resources
	stored := nodeClaim.DeepCopy()
	if nodeClaim.spec.disruption.customFinalizerName != nil {
    controllerutil.AddFinalizer(nodeClaim, nodeClaim.spec.disruption.customFinalizerName) 
	} else {
		controllerutil.AddFinalizer(nodeClaim, v1beta1.TerminationFinalizer) 
	}...
}
```

Node Registration
https://github.com/kubernetes-sigs/karpenter/blob/3c16d48c27cf25338a6f1f634ffb30a1fd03d7d9/pkg/controllers/nodeclaim/lifecycle/registration.go#L81
```go
func (r *Registration) syncNode(ctx context.Context, nodeClaim *v1beta1.NodeClaim, node *v1.Node) error {
	stored := node.DeepCopy()
	if nodeClaim.spec.disruption.customFinalizerName != nil {
    controllerutil.AddFinalizer(node, nodeClaim.spec.disruption.customFinalizerName) 
	} else {
		controllerutil.AddFinalizer(node, v1beta1.TerminationFinalizer) 
	}...
}
```

### Custom Finalizer behavior/ expectation
During termination, Karpenter will not perform the finalization for both node claims and nodes but allow the user's custom controller to perform 
the finalization of node and nodeclaim. We will allow users to set a grace termination period from [#834](https://github.com/kubernetes-sigs/karpenter/pull/834) where 
if set the and breached then Karpenter's termination will occur ungracefully and force terminate both the node and node claim. 

##Concerns
- I am not completely sure if allowing users have control over the finalizer of nodeclaim fully make sense so I am open to changing this. It could be that user defines
a custom finalizer just at the node level and when Karpenter addFinalizer on nodes it uses the custom finalizer rather than the one provided by Karpenter
- Alternatively Karpenter could provide an option where no node finalizer is set by Karpenter and that way it allows the users to inject their own finalizer without 
worrying about compatibility issues with Karpenter's node finalizer
