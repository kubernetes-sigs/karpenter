## Feature Lifecycle

This document describes how a feature request moves from idea to merged code in Karpenter. Understanding this process helps you invest your time effectively.

### 1. Open an Issue

All features start as a GitHub issue. Use the feature request template and describe the problem you're trying to solve, not just the solution you have in mind.

Search existing issues first — your idea may already be tracked. If it is, add your use case as a comment and upvote the issue with a thumbs-up reaction.

You are encouraged to bring your feature request to the [bi-weekly working group meeting](https://karpenter.sh/docs/contributing/community-meetings/) for discussion. This helps maintainers understand your use case and gives you early signal on whether the idea fits the project before you invest further effort.

### 2. Triage

Maintainers evaluate feature requests during [issue triage meetings](https://github.com/kubernetes-sigs/karpenter#issue-triage-meetings). After triage, an issue will be in one of these states:

| State | Labels | What it means |
|-------|--------|---------------|
| Accepted | `triage/accepted` | The feature is in scope |
| Needs information | `triage/needs-information` | We can't evaluate this yet — the reporter needs to clarify |
| Out of scope | Issue closed | The feature doesn't fit the project's scope (see [Scope Guidelines](SCOPE.md)) |

If you would like to participate in the triaging process, attending [issue triage](https://github.com/kubernetes-sigs/karpenter#issue-triage-meetings) will let your voice be heard during the process.

### 3. Check the Priority

Once a feature is accepted, check whether it is labelled `backlog`:

- **No `backlog` label** — You can submit an RFC/PR for this issue.
- **`backlog`** — We want to do this, but aren't accepting contributions for it right now. An RFC/PR will likely not get reviewed if submitted.

If a feature has an **assignee**, that means someone is actively working on it.

If a feature is labelled `maintainer-owned`, a maintainer is driving the feature and we won't accept a contribution for it. Issues labelled `help-wanted` are especially good for community contributions.

If a feature is in `backlog` and you want it moved up, add your use case to the issue and bring it to the [working group meeting](https://karpenter.sh/docs/contributing/community-meetings/) or [issue triage](https://github.com/kubernetes-sigs/karpenter#issue-triage-meetings). More voices and concrete user stories help maintainers reprioritize.

### 4. Request for Comments (`needs-design`)

Maintainers determine whether a feature requires an RFC. Issues that require an RFC will be labelled `needs-design`. The following guidelines indicate when an RFC is likely required:

1. **API changes** — Adding, removing, or modifying fields on any Karpenter CRD (NodePool, NodeClaim, etc.), introducing new custom resources, or changing cloud provider interface signatures.

2. **User-visible behavior changes** — Altering how Karpenter schedules, provisions, disrupts, or repairs nodes in ways users will observe. If a user would need to change how they operate Karpenter, or would notice different behavior after upgrading, an RFC is likely needed.

3. **Significant architectural changes** — Introducing new controllers, replacing core algorithms (e.g. scheduling, bin-packing), unifying or splitting existing controllers, or redesigning internal data models.

#### Submitting an RFC

1. Write your proposal following the [RFC template](designs/rfc-template.md). The other designs in the [designs/](designs/) folder are good prior art to understand how RFCs are written.
2. Open a PR to the `designs/` folder
3. Gather broad feedback — surface your RFC at the [working group meeting](https://karpenter.sh/docs/contributing/community-meetings/), in the [#karpenter Slack channel](https://kubernetes.slack.com/archives/C02SFFZSA2K), and with any Kubernetes SIGs whose scope your design touches. The bigger the change, the more likely it has broader implications than intended.
4. Iterate on feedback until maintainers approve the RFC PR

Once the RFC PR is merged, the issue will be labelled `needs-implementation` and `needs-design` will be removed. The feature is now ready for code.

### 5. Implementation (`needs-implementation`)

Once an RFC is merged (or if no RFC is required):

1. Open a PR referencing the feature issue
2. Follow [conventional commits](https://www.conventionalcommits.org/en/v1.0.0/) for the PR title
3. Ensure tests cover the new behavior
4. Update documentation if the change is user-facing

Large features may be split across multiple PRs. This is encouraged — smaller PRs are easier to review and less risky to merge.

We welcome reviews from all contributors and users, not just maintainers. If you see a PR and have feedback, please share it.

### 6. Merge

A maintainer approves and merges the PR. The feature issue is closed when the implementation is complete.

See [RELEASE.md](RELEASE.md) for information on release cadence and how merged features are published.
