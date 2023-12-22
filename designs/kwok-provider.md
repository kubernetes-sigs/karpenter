# Karpenter KwoK Cloud Provider - v0.34+

The kubernetes-sigs/karpenter repo uses a simulated kubernetes environment to test code changes. Users must use a cloud provider of their choice to test their changes, which can be costly and creates an unreasonable barrier to entry for contributors. Related issue: https://github.com/kubernetes-sigs/karpenter/issues/895.

Creating a Cloud Provider neutral implementation of Karpenter within Karpenter-core requires creating a set of CP neutral examples of instance types and pricing that captures common tenets across CPs. 

This is meant to be a living document to in-depth go through the decisions on how kubernetes-sigs/karpenter models its fake instance types. 

## Proposal

### Instance Types
The full set of instance types for the KwoK Cloud Provider should be at least the cartesian product of the following dimensions, generating 60 instance types across 4 zones, totaling `12 * 3 * 2 = 64` in-memory Cloud Provider instance types.

```md
1. Instance Type Name: (family)-(size)
    a. size: 1x, 2x, 4x, 8x, 16x, 32x, 48x, 64x, 96x, 128x, 192x, 256x
    b. family: c (cpu), s (standard), m (memory)
        i. c ---> 1 vCPU : 2 GiB
        ii. s --> 1 vCPU : 4 GiB
        iii. m -> 1 vCPU : 8 GiB
2. Capacity Type: [on-demand, spot]
```

#### Size
While AWS uses `medium`, `large`, `xlarge`, `2xlarge`, `4xlarge` and so on, AKS (include cpu value in name) and GCE (suffixed with cpu value) use a different naming convention, generally increasing in offering sizes by a factor of 1.5x or 2x.

#### Ratios
AWS Instance Types fall under many "Instance Families" that give a high level overview to why a user may want to use the instance type. AWS `m` instance types are labeled General Purpose, and `c` and `r` represent Compute Optimized and Memory Optimized, most easily compared by the ratio of Memory per vCPU. Defining these ratios is important to test Karpenter's bin-packing value, where applications with more resource-skewed requirements can be more cost effectively bin-packed. 

These ratios seem to accurately depict a common ratios, where "standard/general" ratios for different cloud providers are seen below:
- AWS: General     = 1 vCPU : 4 GiB
- AKS: D Family    = 2 vCPU : 8 GiB
- GKE: C3 Standard = 4 vCPU : 16 GiB

#### Capacity Type Names
Karpenter v1beta1 APIs use `on-demand` and `spot` as the options for `karpenter.sh/capacity-type`, reflecting how they're referenced in AWS and elsewhere. In my docs search, I've found that Cloud Providers have different names for on-demand (e.g. Regular for AKS and Standard for GKE). Since this is how Karpenter defines the capacity type labels, the KwoK CP will use the values defined in the project, regardless of how this value changes in the future.

### Pricing
The pricing for each of the instance types will be a function of the size and capacity type.

EKS: Discount is [< 90%](https://aws.amazon.com/ec2/spot/pricing/#:~:text=Spot%20Instances%20are%20available%20at%20a%20discount%20of%20up%20to%2090%25%20off%20compared%20to%20On%2DDemand%20pricing.)
AKS: Discount is [48-90%](https://azure.microsoft.com/en-us/pricing/details/virtual-machine-scale-sets/linux/)
GKE: Discount is [60-91%](https://cloud.google.com/compute/docs/instances/create-use-spot#:~:text=Spot%20VMs%20are%20available%20at%20a%2060%2D91%25%20discount%20compared%20to%20the%20price%20of%20standard%20VMs.)

We'll define the pricing functions:

```markdown
1. price_of_base_size = (#vCPU * 0.25) + (#GiB * 0.01) # Roughly calculated based on guessing pricing functions from multiple cloud providers. 
2. on_demand_price    = size * price_of_base_size()
3. spot_price         = on_demand_price * .7 # Some discount factor to reflect savings somewhere in the middle of the referenced CPs.
```