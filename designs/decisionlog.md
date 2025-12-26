# Consolidation Savings Threshold: Decision Log

This log tracks the rationale behind design decisions for the consolidation savings threshold RFC.

---

## Decision 001: Document structure - Context before Problem

**Date:** 2025-12-25
**Time:** 18:45 UTC

**What we noticed:**
The Karpenter design guide specifies Context should precede Problem Statement. The current RFC has Problem Statement first. The spot-consolidation RFC also has Problem first, but for this RFC the ordering matters more because Context introduces disruption_cost and lifetime_remaining which are load-bearing concepts readers need before understanding the problem.

**What we will do:**
Reorder the RFC to place Context before Problem Statement.

**How we feel after:**
Done. Context now comes first, introducing disruption_cost and lifetime_remaining before they're needed.

---

## Decision 002: Guiding principles for evaluation

**Date:** 2025-12-25
**Time:** 18:45 UTC

**What we noticed:**
The design guide says Problem should include guiding principles for evaluation. The RFC lacked explicit success criteria beyond "less churn."

**What we will do:**
Add three guiding principles:
1. Reuse calculations from existing code (disruption cost, lifetime remaining) to maintain consistency with established Karpenter behavior
2. Fix issue #7146 in a way that allows other customers to fix similar issues in the future (general mechanism, not special-case)
3. Minimize configurable parameters (one threshold, not a family of knobs)

**How we feel after:**
Done. Added guiding principles section to Problem Statement.

---

## Decision 003: Diagram approach - enhance existing, don't replace

**Date:** 2025-12-25
**Time:** 18:46 UTC

**What we noticed:**
Design guide calls for diagrams. RFC has an ASCII pipeline diagram showing where threshold filtering fits. Critic suggested adding a decision flowchart.

**What we will do:**
Enhance the existing pipeline diagram rather than adding a second one. Space is limited and one clear diagram is better than two competing ones.

**How we feel after:**
Done. Added candidate count annotations showing 100 -> 50 -> 38 -> 15 flow.

---

## Decision 004: Threshold value 0.01 calibration

**Date:** 2025-12-25
**Time:** 18:46 UTC

**What we noticed:**
The 0.01 threshold was criticized as "hand-wavy." But examining spot-consolidation.md, the "15 instance types" threshold has the same character: "Minimum flexibility of 15 spot instance types is decided after an analysis done on the flexibility of AWS customers request in the launch path." No derivation is shown. Why not 10? Why not 20?

Both thresholds are reasonable defaults chosen to be conservative (high enough to prevent the bad behavior, low enough to allow legitimate consolidation). Neither has a rigorous derivation because none exists - these are tunable parameters where the "right" value is workload-dependent.

**What we will do:**
Acknowledge in the RFC that 0.01 is a conservative starting point calibrated against observed instance price gaps, similar to how spot-consolidation's "15" is calibrated against observed customer flexibility patterns. Both can be tuned as we learn from production feedback. Write with the same confidence level as the spot RFC.

**How we feel after:**
Done. Reworded to match spot RFC's confidence level.

---

## Decision 005: Consolidate-delete cliff behavior is correct

**Date:** 2025-12-25
**Time:** 18:47 UTC

**What we noticed:**
Critic raised concern that consolidate-delete creates a "cliff" where cheap dense nodes can never be deleted. Example: $0.10/hr node with 20 pods requires $0.20/hr savings, but deletion only saves $0.10/hr.

Counter-argument: We either believe the disruption cost calculation or we don't. If we don't believe it, we should fix the disruption cost formula because it's used throughout Karpenter. If we do believe it, then the threshold correctly says "disrupting 20 pods requires proportionally more savings than disrupting 5 pods."

The implicit assumption that "pods moving is free" is wrong. Pod restarts have real costs: connection drains, cache warming, state reconstruction, potential brief unavailability. The disruption cost formula attempts to capture this. If a $0.10/hr node with 20 pods can't be deleted for $0.10/hr savings, that's the system working as intended - the savings don't justify the disruption.

If the node naturally drains to 10 pods, deletion becomes viable ($0.10/hr >= $0.10/hr required). If pods are truly movable at no cost, users can lower their threshold.

**What we will do:**
Keep the consolidate-delete behavior as designed. Add explanation that this is intentional: the threshold applies a consistent standard regardless of whether consolidation is replace or delete. Clarify that users who disagree can set threshold to 0.

**How we feel after:**
Done. Added explanation that disruption cost must be believed consistently, and that pod moves have real costs.

---

## Decision 006: Explain lifetime_remaining calculation

**Date:** 2025-12-25
**Time:** 18:47 UTC

**What we noticed:**
The RFC mentions lifetime_remaining multiplier but doesn't explain it. This is a critical part of the algorithm.

**What we will do:**
Add brief explanation: lifetime_remaining ranges from 1.0 (node just launched) to 0.0 (node at expiration). This linearly scales disruption cost, making nodes progressively easier to consolidate as they approach their configured TTL. A node 90% through its lifetime has 0.1 multiplier, reducing required savings by 10x.

Reference pkg/utils/disruption/disruption.go for implementation details.

**How we feel after:**
Done. Added "Lifetime remaining" subsection in Context.

---

## Decision 007: Explain disruption cost at two levels of detail

**Date:** 2025-12-25
**Time:** 18:48 UTC

**What we noticed:**
Tension between "explain the formula" and "keep it simple." The per_pod_cost formula has magic numbers (2^25, 2^27) that are implementation details.

**What we will do:**
Provide both levels:
1. Simple explanation: "For default-priority pods, disruption cost approximates pod count. Higher-priority pods contribute more to disruption cost; lower-priority pods contribute less."
2. Formula for those who want precision, with note that magic numbers are tuned to provide reasonable scaling within the priority range.

**How we feel after:**
Done. Context now has simple explanation followed by formula with note about magic numbers.

---

## Decision 008: Spot threshold interaction is intentional, not ambiguous

**Date:** 2025-12-25
**Time:** 18:48 UTC

**What we noticed:**
Critic marked the spot interaction edge case with "???" suggesting ambiguity. But there's no ambiguity: if threshold filtering leaves fewer than 15 candidates, no spot-to-spot consolidation occurs.

The two mechanisms are complementary:
- Savings threshold: "don't consolidate unless savings justify disruption"
- 15-candidate minimum: "don't consolidate spot unless we have flexibility to avoid racing to bottom"

If a user sets a high threshold that filters out most candidates, they've expressed that they value stability over cost savings. Respecting that choice is correct.

**What we will do:**
Clarify in the RFC that this interaction is intentional. Add worked example showing the case where threshold filtering leaves <15 candidates and consolidation correctly does not proceed. Remove any suggestion of ambiguity.

**How we feel after:**
Done. Added edge case example with 14 candidates remaining after threshold filter, showing NO CONSOLIDATE result.

---

## Decision 009: Default threshold to 0.01

**Date:** 2025-12-25
**Time:** 18:49 UTC

**What we noticed:**
Previous draft defaulted to 0 for alpha (preserving existing behavior). This means the feature does nothing unless users discover and configure it. Counter-argument: if the problem is real, ship the fix.

Spot-consolidation shipped with behavior enabled (15-candidate minimum), not defaulted to "off."

**What we will do:**
Default to 0.01 for alpha. Users who want legacy behavior can set threshold to 0. This matches how spot-consolidation shipped and ensures we get production feedback on the actual feature.

**How we feel after:**
Done. Changed Lifecycle section to default 0.01.

---

## Decision 010: Cloud provider variance - same standard as spot RFC

**Date:** 2025-12-25
**Time:** 18:49 UTC

**What we noticed:**
Design guide says to identify cloud provider variance. Critic noted "spot" is AWS terminology.

However, spot-consolidation.md also uses AWS-specific terminology throughout without extensive discussion of Azure/GCP equivalents. We should not hold this RFC to a higher standard than its predecessor.

The threshold mechanism itself is cloud-agnostic (it's arithmetic on prices and pod counts). Spot allocation strategies vary by provider, but that variance is already handled by spot-consolidation.md.

**What we will do:**
Mention in passing that "spot" terminology follows AWS conventions; other providers have equivalents. Do not belabor the point. Match spot-consolidation's level of detail on this topic.

**How we feel after:**
No change needed. The RFC already uses "spot" without over-explaining. Same standard as spot-consolidation.md.

---

## Decision 011: Remove "Seeking Feedback" section, integrate into Recommendation

**Date:** 2025-12-25
**Time:** 18:50 UTC

**What we noticed:**
Previous round of feedback said to remove the open questions section. Current round suggested making it more prominent. This back-and-forth indicates the section's purpose is unclear.

**What we will do:**
Remove the standalone "Seeking feedback" blockquote. The feedback we want is implicit in the design: reviewers should comment if they disagree with the threshold value, the consolidate-delete behavior, or the spot interaction. The RFC itself is a request for feedback; we don't need to ask twice.

If specific questions arise during working group review, we'll add them back with more context.

**How we feel after:**
Done. Removed the blockquote.

---

## Decision 012: Performance claim scoping

**Date:** 2025-12-25
**Time:** 18:50 UTC

**What we noticed:**
RFC claimed "O(1) arithmetic per node; no measurable impact." This is true but potentially misleading since it only describes the threshold check, not overall consolidation.

**What we will do:**
Scope the claim more precisely: "The threshold comparison itself is O(1). Filtering candidates by threshold is O(candidates). This adds negligible overhead to the existing consolidation pipeline."

**How we feel after:**
Done. Replaced vague claim with precise scoping.

---

## Decision 013: Multi-node consolidation sums disruption costs

**Date:** 2025-12-25
**Time:** 18:51 UTC

**What we noticed:**
Critic questioned why multi-node consolidation sums disruption costs rather than taking max. The sum is correct because:

1. When consolidating nodes A and B together, both sets of pods are disrupted in the same operation
2. The disruptions are not separable - you can't consolidate A without also consolidating B in a multi-node action
3. Total disruption = pods from A + pods from B, therefore total required savings should scale with combined pod count

If the operations were separable (consolidate A, then consolidate B), they would be evaluated as two single-node consolidations with independent thresholds.

**What we will do:**
Add explanation in the multi-node example: "When consolidating multiple nodes in a single operation, disruption costs sum because all pods are disrupted together. This is distinct from sequential single-node consolidations, which are evaluated independently."

**How we feel after:**
Done. Added explanation after the multi-node example.

---

## Decision 014: Be more honest about 0.01 calibration

**Date:** 2025-12-25
**Time:** 19:15 UTC

**What we noticed:**
The RFC claimed 0.01 was "calibrated against observed price gaps" but the calibration story was too confident. Showing that 0.01 *produces* certain outcomes (blocks m8i-to-c8i, allows r8i-to-m8i) doesn't demonstrate those outcomes are *desirable*. The spot-consolidation RFC's "15" has the same character - it's a reasonable guess, not a derivation.

**What we will do:**
Reframe the calibration as a conservative starting point rather than a derived value. "High enough to prevent marginal-savings churn, low enough to allow meaningful consolidation. The exact value is less important than having a non-zero default." Also update Appendix A to acknowledge that whether the default behavior is "right" depends on the workload.

**How we feel after:**
Done. More honest framing. Users can tune if default doesn't suit them.

---

## Decision 015: Clarify which 15 candidates go to PCO

**Date:** 2025-12-25
**Time:** 19:16 UTC

**What we noticed:**
The pipeline diagram showed 38 candidates passing threshold, then "Cap to 15 types for launch" - but didn't say which 15. Cheapest? Highest savings? Random?

**What we will do:**
Clarify in the diagram that it's the cheapest 15 types. This is consistent with existing behavior (we want to send the cheapest viable options to PCO for capacity-aware selection).

**How we feel after:**
Done. Changed "Cap to 15 types" to "Cap to cheapest 15 types".

---

## Decision 016: Add lifetime_remaining escape example for consolidate-delete

**Date:** 2025-12-25
**Time:** 19:17 UTC

**What we noticed:**
The consolidate-delete behavior creates a "cliff" where cheap dense nodes can't be deleted when young. But the lifetime_remaining multiplier provides an escape - as nodes age, they become deletable regardless of pod count. This escape wasn't shown concretely.

**What we will do:**
Add a second worked example showing the same $0.10/hr, 20-pod node at 90% lifetime. With lifetime_remaining = 0.1, disruption cost drops to 2.0, required savings to $0.02/hr, and deletion proceeds.

**How we feel after:**
Done. The escape valve is now visible in the RFC.

---

## Decision 017: Mention low-priority pods reduce disruption cost

**Date:** 2025-12-25
**Time:** 19:18 UTC

**What we noticed:**
The per_pod_cost formula can produce values less than 1.0 (even negative, down to -10.0) for low-priority pods. This means nodes with low-priority workloads are easier to consolidate than the examples suggest. The RFC didn't mention this.

**What we will do:**
Add a note in the Context section: "a node with 20 low-priority pods may have a disruption cost well below 20, making it easier to consolidate than the examples suggest."

**How we feel after:**
Done. Low-priority pod behavior is now documented.

---

## Decision 018: Reframe spot edge case commentary

**Date:** 2025-12-25
**Time:** 19:19 UTC

**What we noticed:**
The edge case (14 candidates < 15 required) had commentary saying "if a user sets a high threshold that filters out most candidates, they've expressed that they value stability." But in the example, the user is using the default threshold - they didn't "set" anything. The framing implied user intent that wasn't there.

**What we will do:**
Reframe as: "When threshold filtering and the 15-candidate minimum combine to block consolidation, both conditions are doing their job: the threshold filters low-value moves, the minimum ensures sufficient flexibility for spot. If this blocks more consolidation than desired, lower the threshold."

**How we feel after:**
Done. No longer imputes user intent that may not exist.

---
