# Running a Karpenter Pull Request Before It's Released

You can run a fix or feature from an open pull request without waiting for it to merge and ship. You build a
controller image yourself from the pull request's code and point your existing Helm release at it. You give up the
tested, signed release image. In exchange, you can confirm that the change solves your problem and give the author
feedback while the pull request is still open.

This is less work than it sounds. The image is the normal Karpenter controller with one line of `go.mod` changed, built
with the same tool the project uses for releases. Switching back to your release is one `helm rollback`.

Note that a self-built image is NOT a supported release. Run it in a test cluster first, keep it out of production
unless you accept that risk, and move back to an official release once the change ships.

---

## 1. How Karpenter images are built

Karpenter is split into two repositories. This repository, `kubernetes-sigs/karpenter`, is the core: scheduling,
disruption and the `NodePool` and `NodeClaim` APIs. It has no image of its own. Each cloud provider, such as
[`aws/karpenter-provider-aws`](https://github.com/aws/karpenter-provider-aws), imports the core as the Go module
`sigs.k8s.io/karpenter` and builds the controller image you install.

So the image you build always comes from a provider repository. For a core pull request, you tell the provider's
`go.mod` to use the pull request's commit instead of the released core. For a provider pull request, you check out the
pull request itself. Either way the image is built with [ko](https://ko.build), which compiles the Go binary and pushes
it to a container registry without a Dockerfile.

The examples below use the AWS provider. Other providers follow the same steps with their own repository, image path
and chart.

---

## 2. Before you start

You need a cluster that already runs Karpenter from the Helm chart, and `kubectl` and `helm` access to it. You also need
[Go](https://go.dev/doc/install), [ko](https://ko.build/install/), Helm 3.14 or newer, and a container registry that
your cluster's nodes can pull from.

On AWS, a private ECR repository works. Nodes pull the controller image with their node IAM role, which needs
permission to pull from the repository, for example `AmazonEC2ContainerRegistryReadOnly`. The repository can be in a different region from the cluster.

```bash
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export AWS_REGION=us-west-2
export KO_DOCKER_REPO=${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/karpenter-dev
aws ecr create-repository --repository-name karpenter-dev --region ${AWS_REGION}
aws ecr get-login-password --region ${AWS_REGION} | docker login --username AWS --password-stdin ${KO_DOCKER_REPO%%/*}
```

---

## 3. Your first image from a core pull request

Find the pull request's fork and head commit. They're on the pull request page, or:

```bash
gh pr view <PR_NUMBER> -R kubernetes-sigs/karpenter --json headRepositoryOwner,headRefOid \
  --jq '"github.com/\(.headRepositoryOwner.login)/karpenter@\(.headRefOid)"'
```

Clone the provider and point its core dependency at that commit. Use the provider's `main` branch, because core pull
requests are written against core's `main`, and the provider's `main` tracks it.

```bash
git clone https://github.com/aws/karpenter-provider-aws
cd karpenter-provider-aws
go mod edit -replace sigs.k8s.io/karpenter=github.com/<FORK_OWNER>/karpenter@<COMMIT_SHA>  # the only code change
go mod tidy
```

`go mod tidy` resolves the commit to a pseudo-version, so `go.mod` ends with a line like:

```
replace sigs.k8s.io/karpenter => github.com/<FORK_OWNER>/karpenter v0.0.0-20261028235551-586cd80def32
```

Build and push the image. ko prints the image reference, including its digest, on the last line.

```bash
ko build --bare --tags core-pr-<PR_NUMBER> ./cmd/controller
```

```
123456789012.dkr.ecr.us-west-2.amazonaws.com/karpenter-dev:core-pr-<PR_NUMBER>@sha256:e630d2bc...
```

If the pull request changes a CRD (a file under `pkg/apis/crds/` in the pull request), run `make verify` first. It
copies the core CRDs into the provider's `pkg/apis/crds/`. Then apply the CRDs and upgrade your release with the chart
from your clone:

```bash
kubectl apply --server-side -f pkg/apis/crds/
helm upgrade karpenter charts/karpenter --namespace kube-system \
  --reset-then-reuse-values \
  --set controller.image.repository=${KO_DOCKER_REPO} \
  --set controller.image.tag=core-pr-<PR_NUMBER> \
  --set controller.image.digest=sha256:<DIGEST>
```

`--reset-then-reuse-values` keeps your settings (cluster name, IAM role, interruption queue) and picks up defaults for
any values the newer chart added. Use your release's namespace if it isn't `kube-system`.

Check that the new pod is running the new image:

```bash
kubectl -n kube-system rollout status deploy/karpenter
kubectl -n kube-system get pods -l app.kubernetes.io/name=karpenter \
  -o jsonpath='{range .items[*]}{.metadata.name} ready={.status.containerStatuses[0].ready} {.spec.containers[0].image}{"\n"}{end}'
```

```
deployment "karpenter" successfully rolled out
karpenter-7476dbdcfd-trh4c ready=true 123456789012.dkr.ecr.us-west-2.amazonaws.com/karpenter-dev:core-pr-<PR_NUMBER>@sha256:e630d2bc...
```

Then exercise the change the pull request makes, and report what you saw on the pull request.

---

## 4. Images from a provider pull request

For a pull request to the provider itself, check out the pull request instead of editing `go.mod`, then build and
deploy as in [section 3](#3-your-first-image-from-a-core-pull-request):

```bash
git clone https://github.com/aws/karpenter-provider-aws
cd karpenter-provider-aws
git fetch origin pull/<PR_NUMBER>/head
git checkout FETCH_HEAD
ko build --bare --tags provider-pr-<PR_NUMBER> ./cmd/controller
...
```

If the change is already merged to the provider's `main` but not released, you don't need to build anything. The AWS
provider publishes a [snapshot release](https://karpenter.sh/docs/upgrading/compatibility/#snapshot-releases) for every
merged commit.

---

## 5. Going back to a release

Helm keeps every revision of your release. List them and roll back to the one before your upgrade:

```bash
helm -n kube-system history karpenter
helm -n kube-system rollback karpenter <REVISION>
```

`helm rollback` doesn't revert CRDs you applied by hand. If the pull request changed a CRD, apply the CRDs from your
release afterward, for example with the `karpenter-crd` chart at your version.

When the change ships in a release, upgrade to that release the normal way, and delete the image from your registry.

---

## 6. Troubleshooting

If the controller pod restarts with a panic like the one below after `helm upgrade --reuse-values`, the newer chart added
a setting that `--reuse-values` left empty. Upgrade again with `--reset-then-reuse-values`.

```
panic: parsing feature gates, invalid value of PredictionEnabled: , err: strconv.ParseBool: parsing "": invalid syntax
```

If `ko build` fails with the error below, you're building in a `git worktree`. The provider's `cmd/controller/kodata`
links into `.git`, which is a file in a worktree. Build from a normal clone.

```
tarring kodata: filepath.EvalSymlinks(".../cmd/controller/kodata/HEAD"): not a directory
```

If `go build` or `ko build` fails to compile after the `replace`, the provider and the core pull request have drifted
apart. Make sure you're on the provider's `main`. If it still fails, the core change needs a matching provider change,
so ask on the pull request.

If the pod stays in `ImagePullBackOff`, the node can't pull from your registry. Check the node role's registry
permissions and that the image reference includes the digest ko printed.
