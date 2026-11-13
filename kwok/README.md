# Kwok Provider

Before using the kwok provider, make sure that you don't have an installed version of Karpenter in your cluster.

## Requirements
- Have an image repository that you can build, push, and pull images from.
  - For an example on how to set up an image repository refer to [karpenter.sh](https://karpenter.sh/docs/contributing/development-guide/#environment-specific-setup)
- Have a cluster that you can install Karpenter on to.
  - For an example on how to make a cluster in AWS, refer to [karpenter.sh](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)

If you use a kind cluster, please set the following environment variables:
```bash
export KWOK_REPO=kind.local
export KIND_CLUSTER_NAME=<kind cluster name, for example, chart-testing>
```

## Installing
```bash
make install-kwok
make apply # Run this command again to redeploy if the code has changed
```

## Create a NodePool

Once kwok is installed and Karpenter successfully applies to the cluster, you should now be able to create a NodePool.

```bash
cat <<EOF | envsubst | kubectl apply -f -
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
        - key: kubernetes.io/os
          operator: In
          values: ["linux"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot"]
      nodeClassRef:
        name: default
        kind: KWOKNodeClass
        group: karpenter.kwok.sh
      expireAfter: 720h # 30 * 24h = 720h
  limits:
    cpu: 1000
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 10s
---
apiVersion: karpenter.kwok.sh/v1alpha1
kind: KWOKNodeClass
metadata:
  name: default
EOF
```

## Taint the existing nodes

```bash
kubectl taint nodes <existing node name> CriticalAddonsOnly:NoSchedule
```
After doing this, you can create a deployment to test node scaling with kwok provider.

## Specifying Instance Types

By default, the KWOK provider will create a hypothetical set of instance types that it uses for node provisioning.  You
can specify a custom set of instance types by providing a JSON file with the list of supported instance options. To do so,
set the `--instance-types-file-path` flag or `INSTANCE_TYPES_FILE_PATH` environment variable to your custom file's path.

There is an example instance types file in [examples/instance\_types.json](examples/instance_types.json) that you can
regenerate with `make gen_instance_types`.

## Controller retry configuration

Pass `controllers.WithRetryConfig(controllers.RetryConfiguration{...})` to
`controllers.NewControllers` to set retries per controller. Each entry uses
`pkg/utils/controller.RetryConfig`. No operator flags are needed.

KWOK's [`main.go`](main.go) sets all four fields for `NodeClaimLifecycle` and
`NodeTermination`. Zero-valued fields keep the controller's defaults, including
concurrency-derived QPS and burst. Direct controller constructors also accept optional
`WithRetryConfig(...)` options; omit them to keep the defaults.

- `InitialDelay`: per-resource delay after the first failed reconciliation.
- `MaxDelay`: maximum per-resource exponential backoff.
- `QPS`: controller-wide token-bucket retry scheduling rate.
- `Burst`: token-bucket capacity.

Invalid settings fail registration. After defaults apply, delays and burst must be positive,
QPS must be positive and finite, and `InitialDelay` must not exceed `MaxDelay`.

Settings cover all failed reconciliations, so lifecycle shares them between creation and
deletion. The longer of the backoff and bucket delays wins; bucket waits can exceed
`MaxDelay`. Watch events can trigger earlier reconciliations. These settings do not enforce
cloud-operation cooldowns or account-wide API limits.

## Testing

To test the provider, run `make e2etests` in the root of the repository.

If you want to test a specific e2e case, run the following command:
```bash
export FOCUS="<e2e case name>"
make e2etests
```

## Notes
The kwok provider will have additional labels `karpenter.kwok.sh/instance-type`, `karpenter.kwok.sh/instance-size`,
`karpenter.kwok.sh/instance-family`, `karpenter.kwok.sh/instance-cpu`, and `karpenter.sh/instance-memory`. These are
only available in the kwok provider to select fake generated instance types. These labels will not work with a real
Karpenter installation.

## Uninstalling
```bash
make delete
make uninstall-kwok
```
