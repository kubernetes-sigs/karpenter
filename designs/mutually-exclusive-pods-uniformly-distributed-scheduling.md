# Mutually Exclusive Pods Uniformly Distributed Scheduling

## Background

> [issues 1418](https://github.com/kubernetes-sigs/karpenter/issues/1418)

I have 10 pending pods:

- **pod1**: 1c1g requests, with anti-affinity; cannot be scheduled on the same node as pod10 and pod9.
- **pod2 ~ pod8**: 1c1g requests; no anti-affinity is configured.
- **pod9**: 1c1g requests, with anti-affinity; cannot be scheduled on the same node as pod1 and pod10.
- **pod10**: 1c1g requests, with anti-affinity; cannot be scheduled on the same node as pod1 and pod9.

In Karpenter, it creates three nodes, with the first node being super large and the remaining two being very small:

- **node1**: c7a.4xlarge, 16c32g (8Pods)
- **node2**: c7a.large, 2c4g (1Pod)
- **node3**: c7a.large, 2c4g (1Pod)

**Expected Behavior**:
I want the resources of the three nodes to be evenly distributed, like:

- **node1**: c7a.4xlarge, 8c16g (4Pod)
- **node2**: c7a.xlarge, 4c8g (3Pod)
- **node3**: c7a.xlarge, 4c8g (3Pod)

In this situation, the cluster will be more stable (e.g., draining one node will not cause most pods to be rescheduled).

## Proposal Solution

By sorting in byCPUAndMemoryDescending, pods with the same specifications (cpu or memory request equal) and mutually exclusive pods (with PodAntiAffinity or TopologySpreadConstraints) are prioritized to be scheduled first.

By adding sort rules, priority scheduling with mutually exclusive Pods, so that pods more evenly distributed, to avoid over-inflation of a single node, node specifications more balanced.

Code implementation: [pull 1548](https://github.com/kubernetes-sigs/karpenter/pull/1548)

### Test Results

```bash
❯ kubectl get nodeclaims
NAME            TYPE               CAPACITY   ZONE          NODE                             READY   AGE
default-8wq87   c-8x-amd64-linux   spot       test-zone-d   blissful-goldwasser-3014441860   True    67s
default-chvld   c-4x-amd64-linux   spot       test-zone-b   exciting-wescoff-4170611030      True    67s
default-kbr7n   c-2x-amd64-linux   spot       test-zone-d   vibrant-aryabhata-969189106      True    67s
❯ kubectl get pod -owide
NAME                       READY   STATUS    RESTARTS   AGE   IP           NODE                             NOMINATED NODE   READINESS GATES
nginx1-67877d4f4d-nbmj7    1/1     Running   0          77s   10.244.1.0   vibrant-aryabhata-969189106      <none>           <none>
nginx10-6685645984-sjftg   1/1     Running   0          76s   10.244.2.2   exciting-wescoff-4170611030      <none>           <none>
nginx2-5f45bfcb5b-flrlw    1/1     Running   0          77s   10.244.2.0   exciting-wescoff-4170611030      <none>           <none>
nginx3-6b5495bfff-xt7d9    1/1     Running   0          77s   10.244.2.1   exciting-wescoff-4170611030      <none>           <none>
nginx4-7bdd687bb6-nzc8f    1/1     Running   0          77s   10.244.3.5   blissful-goldwasser-3014441860   <none>           <none>
nginx5-6b5d886fc7-6m57l    1/1     Running   0          77s   10.244.3.0   blissful-goldwasser-3014441860   <none>           <none>
nginx6-bd5d6b9fb-x6lkq     1/1     Running   0          77s   10.244.3.2   blissful-goldwasser-3014441860   <none>           <none>
nginx7-5559545b9f-xs5sm    1/1     Running   0          77s   10.244.3.4   blissful-goldwasser-3014441860   <none>           <none>
nginx8-66bb679c4-zndwz     1/1     Running   0          76s   10.244.3.1   blissful-goldwasser-3014441860   <none>           <none>
nginx9-6c47b869dd-nfds6    1/1     Running   0          76s   10.244.3.3   blissful-goldwasser-3014441860   <none>           <none>
```
