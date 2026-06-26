## Scope Guidelines

Karpenter is a node autoscaler. It provisions compute capacity so pods can run, and removes capacity that is no longer needed. The project maintains a strong bias toward:

- **Saying "no"** to changes that don't clearly demonstrate broad benefit
- **Minimizing API surface** — no API is the best API
- **Simple solutions to complex problems** — features should "just work" without user knobs

### Out-of-Scope Categories

#### 1. Notification and Alerting Delivery

Karpenter emits Kubernetes events, status conditions, and Prometheus metrics. Delivering notifications (webhooks, Slack, email, PagerDuty) is the responsibility of external tooling that consumes these signals.

#### 2. Pod-Level Scheduling Decisions

Karpenter provisions nodes, not pods. Decisions about which pod runs on which node belong to the kube-scheduler. Features that attempt to influence pod placement, emit pod-level events for scheduling decisions, or duplicate scheduler functionality are out of scope.

#### 3. Exposing Internal Implementation Details

Karpenter exposes configuration that lets users express desired behavior — for example, consolidation policy or disruption budgets. What Karpenter does not accept is configuration that exposes internal implementation mechanics, like thread concurrency. Users should express *what* they want Karpenter to do, not *how* it does it internally.

#### 4. Provider-Specific Features

kubernetes-sigs/karpenter is the provider-neutral core. Features that are specific to a single cloud provider belong in that provider's repository (e.g. [karpenter-provider-aws](https://github.com/aws/karpenter-provider-aws), [karpenter-provider-azure](https://github.com/Azure/karpenter-provider-azure)). See the [full list of providers](README.md#karpenter-implementations). Only features that apply across all provider implementations belong here.

#### 5. Workload Management

Karpenter is not a workload manager. It is not responsible for scaling workloads or pods. Features that manage pod replicas, autoscale deployments, or otherwise control workload lifecycle belong in other systems (e.g. HPA, KEDA, VPA).
