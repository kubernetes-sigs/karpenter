# Custom Node termination (custom finalizer)

## Motivation
Users are interested in customizing how node terminations occurs and although the existing finalizer is very useful its difficult introduce any custom termination
logic for nodes [#743](https://github.com/kubernetes-sigs/karpenter/issues/743), [#622](https://github.com/kubernetes-sigs/karpenter/issues/622), [#740](https://github.com/kubernetes-sigs/karpenter/issues/740), [#690](https://github.com/kubernetes-sigs/karpenter/issues/690)
[#5232](https://github.com/aws/karpenter/issues/5232)
* Cluster admins may have unique methods for draining or terminating nodes outside of what Karpenter currently supports. It could be because it depends other signals,
or more complicated termination process. We want to give cluster admins this flexibility

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
    customFinalizerName: example.com/customNodeTermination || None
```


## Implementation

### NodeClass 

https://github.com/kubernetes-sigs/karpenter/blob/3c16d48c27cf25338a6f1f634ffb30a1fd03d7d9/pkg/controllers/nodeclaim/lifecycle/controller.go#L87
```go

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	// Add the finalizer immediately since we shouldn't launch if we don't yet have the finalizer.
	// Otherwise, we could leak resources
	stored := nodeClaim.DeepCopy()
	if nodeClaim.spec.disruption.customFinalizerName != nil &&  nodeClaim.spec.disruption.customFinalizerName != "None" {
    controllerutil.AddFinalizer(nodeClaim, nodeClaim.spec.disruption.customFinalizerName) 
  } else {
	  controllerutil.AddFinalizer(nodeClaim, v1beta1.TerminationFinalizer) 
  }...
```

Check for custom finalizer and writes that object instead of default finalizer.  

### Termination Controller Behavior


### Force Drain Behavior
Drain behavior should be similar to using kubectl to bypass standard eviction protections.
```
$ kubectl drain my-node --grace-period=0 --disable-eviction=true

$ kubectl drain --help
Options:
    --grace-period=-1:
	Period of time in seconds given to each pod to terminate gracefully. If negative, the default value specified
	in the pod will be used.

    --disable-eviction=false:
	Force drain to use delete, even if eviction is supported. This will bypass checking PodDisruptionBudgets, use
	with caution.
```

It might sufficient to skip straight to termination of the node at the cloud provider rather than waiting for a successful force drain to complete.
