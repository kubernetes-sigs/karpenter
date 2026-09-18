# Contributing to Karpenter

Welcome to Karpenter! We are excited about the prospect of you joining our [community](https://git.k8s.io/community). The Kubernetes community abides by the CNCF [code of conduct](code-of-conduct.md).

## Getting Started

- [Contributor License Agreement](https://git.k8s.io/community/CLA.md) - You must sign a CLA before we can accept your pull requests
- [Kubernetes Contributor Guide](https://k8s.dev/guide) - Main contributor documentation
- [Contributor Cheat Sheet](https://k8s.dev/cheatsheet) - Common resources for existing developers
- [Mentoring Initiatives](https://k8s.dev/community/mentoring) - Programs available for new contributors

The best way to get involved is to attend our [working group meetings](README.md#working-group-meetings). These are open to everyone and are where design decisions, feature discussions, and release planning happen.

## Finding Work

Issues labelled [`help-wanted`](https://github.com/kubernetes-sigs/karpenter/labels/help%20wanted) are good starting points for contributions. If you want to work on an issue, assign yourself to indicate that you're working on it. Assigning is not authoritative — it's a heads up to everyone else.

### Features

Features follow a structured lifecycle from proposal to implementation. Before starting work on a feature, read the [Feature Lifecycle](FEATURE_LIFECYCLE.md) and [Scope Guidelines](SCOPE.md).

### Bug Fixes, Refactors, Tests, and Documentation

For non-feature contributions, open a pull request referencing the relevant issue. If no issue exists, create one first for bug fixes so the problem can be discussed. For test improvements or documentation changes, a PR without a prior issue is fine.

## Pull Request Guidelines

- Use [conventional commits](https://www.conventionalcommits.org/en/v1.0.0/) for your PR title (`feat:`, `fix:`, `chore:`, `perf:`, `docs:`, `test:`, `ci:`)
- Reference the issue your PR addresses
- Ensure tests cover any behavior changes
- Update documentation for user-facing changes
- Large changes should be split into smaller PRs when possible

## Key Documents

| Document | Purpose |
|----------|---------|
| [Feature Lifecycle](FEATURE_LIFECYCLE.md) | How features are proposed, evaluated, and tracked |
| [Scope Guidelines](SCOPE.md) | What Karpenter will and won't do |
| [RFC Template](designs/rfc-template.md) | Template for design proposals |
| [Contributor Ladder](contributing-guidelines.md) | Path from contributor to reviewer to approver |
| [Release Process](RELEASE.md) | How releases are versioned and published |
| [Development Guide](https://karpenter.sh/docs/contributing/development-guide/) | Setting up your local environment |

## Contact Information

For questions about using or deploying Karpenter, reach out in [#karpenter](https://kubernetes.slack.com/archives/C02SFFZSA2K) on Kubernetes Slack. For contribution and development discussions, use [#karpenter-dev](https://kubernetes.slack.com/archives/C04JW2J5J5P). See the [README](README.md#community-discussion-contribution-and-support) for meeting schedules and additional resources.
