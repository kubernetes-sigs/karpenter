# Spot Consolidation

## Problem Statement

Customers face increased costs due to underutilized spot instances. A customer with 4 x 24cpu pods would launch a 24xl (96cpu) instance type. If three of the four pods complete, the instance will indefinitely run at 25% utilization until the pod completes, is able to fit elsewhere in the cluster, or additional work is scheduled to the node. Customers have raised this inefficiency as a critical pain point with Karpenter: https://github.com/aws/karpenter-core/issues/763. 

Karpenter’s consolidation algorithm minimizes the price of EC2 instances given pod scheduling constraints. However, price minimization presents challenges for EC2 Spot, which offers a tradeoff between price and interruption rate. Karpenter uses EC2 Spot’s PriceCapacityOptimized (PCO) allocation strategy to provide a balanced approach to this tradeoff, and doesn't always select the cheapest instance type, since it may result in immediate interruption. If Karpenter were to consolidate spot capacity based purely on cost, it would repeatedly interrupt the same instance, walking down the PCO decision ladder, until only the lowest price remained. Because of these complications, Karpenter does not currently support "spot replacement" consolidation.

All consolidation actions contain an inherent tradeoff between price and availability. Consolidation actions are equivalent to a spot interruption, but without the deadline. This tradeoff applies to on-demand capacity, as well. This document explores approaches to enable consolidation for spot instances without regressing to lowest price. 

## Design Options

### 1: Minimum Flexibility [Recommended] 

Karpenter could skip consolidation if the number of potential replacement instance types were below a threshold (e.g. 20). To avoid repeat interruptions, e.g. "walking down the ladder", Karpenter would have to limit its flexibility to this threshold. By ensuring that we have a minimum flexibility in our launch decisions, we can be assured that we have enough instance diversity that it's very unlikely that all instances that we select will have high interruption. Further, Karpenter would have to limit its flexibility during the launch scenario as well, else face an immediate consolidation if the launched instance type’s price were more than the price of any instance type under the flexibility threshold. Since on-demand capacity uses the lowest price strategy, its behavior is unaffected if these rules are applied equally to both spot and on-demand launches.

Conceptually, this approach is straightforward to explain to customers and directly aligned with Spot best practices. Some customers may be disappointed by the flexibility requirement, but given the current lack of support for spot consolidation, it’s net positive from the status quo. While there may be valid use cases for customers to trade interruption rate for lower prices, we should delay enabling customers to configure this value until the use cases are better understood. 

* 👍👍👍 Prevents consolidation from regressing to a lowest price allocation strategy
* 👍 Easy to explain and implements spot best practices
* 👍 Flexibility threshold is tunable, as best practices evolve. Cloud providers could choose to set it to 1 to disable this behavior.
* 👎 Limits potential flexibility from status quo for both instance launch and replacement. Today, Karpenter can send unbounded flexibility to PCO (e.g. 100s of instance types). 
* 👎 Fails to take advantage of opportunities to migrate to healthier pools due to limited instance type flexibility.

### 2. Price Improvement Factor

Karpenter could skip consolidation’s if the replacement instance types were only marginally better in price. For example, if replacing an m5a.large with an m5.large would only save 5%, it may not be worth the interruption. This behavior is valuable both for spot and on-demand instances, given the fact that consolidation is inherently an interruption. PIF must also constrain provisioning decisions, as without this, an instance could be provisioned by PCO, and then immediately consolidated. 

Given the cost of interrupting a workload, as well as the relatively short average lifetime of a workload, this value would need to be significant (e.g. 2x). Further analysis of the right default value function (constant, linear, polynomial) is needed, and given the difficulty of choosing this value, we should think carefully before enabling customers to configure it themselves.

This approach is predicated on customers configuring instance family flexibility, and relatively similar spot prices within a given size. For a customer flexible to all instance families within c,m,r categories, PCO would have a broad range of instance types to choose from, and would be unlikely to be forced to choose an unhealthy pool. This assumption follows EC2 spot best practices, and is relied upon by customers who use attribute-based selection with price protection.

For customers who rely on instance size flexibility within a single family, or lack sufficient flexibility, they may face increased interruption rates compared to options 1 and 2. Due to the natural price differences between different instance sizes within the same family, this approach would limit flexibility to one or two instances sizes, depending on current spot markets. However, having limited flexibility goes against spot best practices, and the customer should expect increased interruptions.

See appendix for examples.

* 👍👍 Limits consolidation from regressing to a lowest price allocation strategy
* 👍 Extensible to new methods of capacity allocation, since Karpenter core is now completely agnostic to spot
* 👍 Prevents consolidation from interrupting workloads for marginal improvement
* 👎 During a spot capacity crunch, if many capacity pools within a price range are unhealthy, the customer may get interrupted soon after the consolidation. This is exacerbated if the customer cannot tolerate instance type diversity. 
* 👎 Tuning this value to migitage both marginal-improvement-churn and lowest-price-regression is challenging, and desired values may conflict.

Note: Regardless of the decision made to solve the spot consolidation problem, we’d likely want to implement a price improvement in the future to prevent consolidation from interrupting nodes to make marginal improvements.

### 3. Optimistically Launch Replacements

Karpenter could optimistically launch new EC2 instances to probe PCO decision making. If PCO launches a cheaper instance type from a CreateFleet request that include the current instance type, we know that the instance is both lower price and lower interruption rate. If the PCO algorithm selects the current instance type of the running instance, Karpenter would terminate the new instance and leave the current instance running. This approach would need to include an optimistic launch cache to minimize the number of optimistic launches.

* 👍👍👍 Prevents consolidation from regressing to a lowest price allocation strategy
* 👎 May not make sense for all cloud providers
* 👎 May consolidate to make marginal improvements, resulting in unnecessary disruption
* 👎 Optimistic launches may confuse customers looking deeply at their bill or audit logs
* 👎 Bugs with this mechanism are higher risk, since real instances are being launched.

### 4. Minimum Node Lifetime

Karpenter could skip consolidation actions if the instance had been launched recently. This wouldn’t prevent the allocation strategy from regressing to lowest price, but given sufficient time (e.g. 4 hours), prevents the worst case scenario of repeated interruption.

There are multiple ways to model this time based constraint. Minimum node lifetime would prevent any consolidation of a node under a given age. Alternatively, this could be modeled as minimum pod lifetime, where consolidation would not terminate a node until all pods are above a specified age. This is the most direct approach to mitigate the problem statement: if there’s too much interruption, reduce frequency. We should delay (if ever) enabling customers from being able to configure this. 

Alternatively, we could rely on ttlSecondsAfterUnderutilized, where Karpenter would detect a potential consolidation and wait for the ttl before taking action. This is somewhat unnatural, since we’ve seen customers configure the similar ttlSecondsAfterEmpty in the range of 30-180 seconds, which isn’t enough time to avoid regression to lowest price. It’s likely that customers would want one value (e.g. 30s) to handle empty nodes and another value to prevent repeated interruption (e.g. 4h). Further, given that we’re building consolidateAfter to meet other cases, customers would be required to reason about both minimum node lifetime and consolidateAfter. 

* 👍 Avoids thrashing and delays regression to a lowest price allocations strategy
* 👎 Eventually converges to a lowest price allocation strategy, which could increase interruptions
* 👎 Consolidatable nodes may sit around for much longer than necessary
* 👎 Increased complexity of additional controls that interact with other disruption behaviors

Note: Minimum node lifetime has other use cases, such minimizing disruption due to “consolidation delete“ actions for recently scheduled pods. We may need to explore similar solutions to this problem in the future.

### Recommendation

Each of these approaches carries tradeoffs. It’s possible to layer multiple of these options for defense in depth against the problem. We could consider an iterative approach and drive these layers once we learn more from customer feedback.

Option 1 is straightforward to explain to customers and can be implemented easily. Options 2 and 4 layer naturally on top of it. It’s likely we’d want to do Option 2 regardless of whether or not Option 1 is a sufficient solution to this problem. It’s unlikely that we’d layer Option 3 with Option 1, as we wouldn’t need to limit flexibility if we optimistically launched capacity — we’d simply know the correct answer. 

## Appendix

### 1. Minimum Flexibility Example

Assume we have instance types sorted in ascending price order I0, I1, ..., I500 and in this example our pods can always schedule on a node if its more expensive than the current one. This isn't realistic, but works for our example.

We have a set of pending pods and need to launch a spot node. The cheapest instance that would suffice is I50, so we send a CreateFleet request with the cheapest type that would work and the next 19 more expensive types that could also work I50, I51, ..., I69. We get back an I55 from PCO.

One pod exits, and now an I40 would work. We don't consolidate spot → spot here since the set of candidates we might consolidate to (I40, I41, ... I59) already includes the current type, an I55. 

Another pod exits, and now an I30 would work. We consolidate spot → spot and send a CreateFleet request for I30, I31, ..., I49.

### 2. Price Factor Example

#### 1. Replacing m5.xlarge w/  m5.* :

m5.xlarge spot price = $0.06
replacement price must be < $0.03
Candidates = 0
Closest Candidate = m5.large at $0.035

#### 2. Replacing m5.8xlarge w/  m5.* :

m5.8xlarge spot price = $0.565
replacement price must be <$0.283
Candidates = 4
 - m5.large ($0.035)
 - m5.xlarge ($0.06)
 - m5.2xlarge ($0.15)
 - m5.4xlarge ($0.24)
