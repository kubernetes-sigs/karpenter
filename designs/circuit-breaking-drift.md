# RFC: Allow blocking NodePool disruption, consolidation and scale down via external signals

## Overview

This design proposes a method to pause (a.k.a circuit-break) Karpenter disruption and consolidation actions with the help of signals outside of Karpenter. Currently, there is no mechanism within Karpenter to turn off disruption/consolidation with the help of an external signal that may have cluster-specific context about health. 

Fixes https://github.com/kubernetes-sigs/karpenter/issues/2497

## User stories  

1. As a cluster operator, I want to provide Karpenter an automated signal based on my own critical business metrics to pause drifts and scale down.  
2. As a Karpenter developer, I want to provide a simple hook-like interface for cluster operators to introduce their cluster-specific logic for pausing/continuing drifts. 


## Problem statement

Karpenter is used as both an Autoscaler as well as a drift corrector for capacity attached to a Kubernetes cluster. Karpenter's powerful semantics on drift resolution such as Node disruption budgets, respecting Pod Disruption Budgets, special annotations for blocking disruptuon etc simplify node management operations for Cluster operators. However, Karpenter's logic is self-contained and lacks any visibility into consequences of drift actions to end-user performance metrics. As a result, there are scenarios where Karpenter may cause drift to proceed too quickly because of its inability to respond to usage specific signals. A few concrete examples:
  
  * An unrelated incident for a critical application running on the cluster requires capacity remain static until Incident mitigation to avoid application pods getting re-homed during the incident. 
  * A new OS/AMI update allow nodes to join the cluster but provides poorer performance to critical business workloads. Stopping further rollout of the 'bad' AMI is critical to avoid further degradation. 
  * A bad change that wipes out NodePool resources from the cluster may cause runaway scale-down by Karpenter but having a cluster alert like "Too many nodes scaling down in a short period" can help circuit break loss of all capacity (True story :()
 
## Proposal

### Interface changes
Provide an optional HTTP probe interface within each NodePool similar to Readiness probes to verify if Disruption or Consolidation is allowed. 

```yaml

kind: NodePool
metadata:
  name: my-node-pool-1
spec: 
  template:
    disruption:
      # NEW!
      disruptionProbes:
      # All probes are HTTP requests modeled after https://pkg.go.dev/golang.org/x/build/kubernetes/api#HTTPGetAction
      - path: "/healthz" # Note: Cluster operators can use more granular paths for specific NodePools if their health probe service supports it.
        port: 8080
        host: "myclusterhealth.svc.local"
        scheme: "http"

```

## Design Options

### Option 1: Response code based interpretation of actions - Recommended 
        
A response code of 200-202 will be accepted as 'healthy' allowing disruption actions to proceed. Any other response code (including inability to communicate with the health service) will block disruptions.  

Pros:
* Simple, familiar interface for Kubernetes users that are used to pod level probes. 
* Interface is sufficiently flexible to customize behaviour per NodePools

Cons:
* Hard to control actions granularly, for example: block disruption but allow consolidation or vice-versa. However, based on user stories so far, this might not be something worth optimizing for.
          
### Option 2: Structured response format from health endpoint

In this option, the response body will be parsed to identify what actions can be taken. A sample response from the service may look like:

```yaml
deniedActions:
  - drift
``` 

Pros:
* More extensible in the future if other actions are supported?

Cons:
* Marginally more complicated to implement.
 

### Option 3: Cluster level actions API (Alternative interface) - Feedback requested

In this option, instead of having a NodePool level health probe, a cluster level probe will be implemented by Cluster operators. The endpoint for this probe would be and exposed as part of Karpenter [settings](https://karpenter.sh/docs/reference/settings/). Operators would implement a single Karpenter Actions API per cluster with a response type as follows:

```yaml
deniedActions:
- name: drift
  clusterWide: false # If flipped to true, the nodePools parameter is ignored and drift is blocked cluster-wide
  nodePools:
    labels: [ "a-specific-az", "my-gpu-nodes", "my-specific-nodepool" ]    
```

Pros:
* Single API call to determine denied drifts / consolidations for the whole cluster.
* Ability to block certain actions at the cluster level. 

Cons:
* Expansion of Karpenter settings which has been communicated previously as undesirable
* Harder to debug reasons for drift failing since no constraints are visible in the NodePool CRD. In a world where NodePools are managed by different personas than clusters (ex. dedicated capacity provisioning teams), this can become a little more confusing. 
