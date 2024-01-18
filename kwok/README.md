# Kwok Provider

Before using the kwok provider, make sure that you don't have an installed version of Karpenter in your cluster. 

## Requirements
- Have an image repository that you can build, push, and pull images from.
- Have a cluster that you can install Karpenter on to.
  - For an example on how to make a cluster in AWS, refer to [karpenter.sh](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)

## Installing
```bash
make install-kwok
make apply
```

## Create a NodePool

Once kwok is installed and Karpenter successfully applies to the cluster, you should now be able to create a NodePool. 

```bash
cat <<EOF | envsubst | kubectl apply -f -
apiVersion: karpenter.sh/v1beta1
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
        name: nil
  limits:
    cpu: 1000
  disruption:
    consolidationPolicy: WhenUnderutilized
    expireAfter: 720h # 30 * 24h = 720h
EOF
```

## Notes
- The kwok provider will have additional labels `karpenter.kwok.sh/instance-size`, `karpenter.kwok.sh/instance-family`, `karpenter.kwok.sh/instance-cpu`, and `karpenter.sh/instance-memory`. These are only available in the kwok provider to select fake generated instance types. These labels will not work with a real Karpenter installation.
- Additionally, this installs Karpenter with a hard-coded set of instance types. A dynamic set of instance types is not supported yet.

## Uninstalling
```bash
make delete
make uninstall-kwok
```