# Release Process

## Overview

The Karpenter project is an SIG autoscaling project that has the following two components:
- Kubernetes Custom Resource Definitions (CRDs)
- Corresponding Go API in the form of [`sigs.k8s.io/karpenter`](https://pkg.go.dev/sigs.k8s.io/karpenter) Go package

This repository is the home for both of the above components.

## Versioning Strategy

The versioning strategy for this project is covered in detail in [the release type section of the compatability documentation](https://karpenter.sh/docs/upgrading/compatibility/#release-types).

## Releasing a New Version

The following steps must be done by one of the [Karpenter Maintainers](https://github.com/kubernetes/org/blob/main/config/kubernetes-sigs/sig-autoscaling/teams.yaml):

For a **MAJOR**, **MINOR**, or **RC** release:
- Verify the CI tests pass before continuing.
- Create a tag using the `HEAD` of the `release-vx.x` branch by using `git tag vx.x.x`
- Push the tag to upstream using `git push upstream vx.x.x`
- This tag will kick-off the [Release Workflow](https://github.com/kubernetes-sigs/karpenter/actions/workflows/release.yaml) which will auto-generate release notes into the repo's [Github releases](https://github.com/kubernetes-sigs/karpenter/releases).

For a **PATCH** release:
- Create a new branch in the upstream repo similar to `release-vx.x`.
- Create a branch in your fork (origin) repo similar to `<githubuser>/release-vx.x.x`. Use the new branch
  in the upcoming steps.
- Use `git` to cherry-pick all relevant PRs into your branch.
- Create a pull request of the `<githubuser>/release-vx.x.x` branch into the `release-vx.x` branch upstream. Wait for at least one maintainer/codeowner to provide a `lgtm`.
- Verify the CI tests pass and merge the PR into `release-vx.x`.
- Create a tag using the `HEAD` of the `release-vx.x` branch by using `git tag vx.x.x`
- Push the tag to upstream using `git push upstream vx.x.x`
- This tag will kick-off the [Release Workflow](https://github.com/kubernetes-sigs/karpenter/actions/workflows/release.yaml) which will auto-generate release notes into the repo's [Github releases](https://github.com/kubernetes-sigs/karpenter/releases).