#!/usr/bin/env bash
set -exuo pipefail

# (maxcao13): K8S_VERSION will follow the version of the k8s.io/api module.
K8S_VERSION=$(go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $3}')
KUBEBUILDER_ASSETS="/usr/local/kubebuilder/bin"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export GOFLAGS="-mod=readonly -modfile=${SCRIPT_DIR}/go.mod"

main() {
    tools
    kubebuilder
}

# (maxcao13): some tools have been disabled from upstream because they are not needed downstream.
tools() {
    go mod tidy -C "${SCRIPT_DIR}"
    # go install github.com/google/go-licenses //disabled
    go install github.com/golangci/golangci-lint/cmd/golangci-lint
    go install github.com/mikefarah/yq/v4
    go install github.com/google/ko
    # go install github.com/norwoodj/helm-docs/cmd/helm-docs //disabled
    go install sigs.k8s.io/controller-runtime/tools/setup-envtest
    go install sigs.k8s.io/controller-tools/cmd/controller-gen
    go install golang.org/x/vuln/cmd/govulncheck
    go install github.com/onsi/ginkgo/v2/ginkgo
    # go install github.com/rhysd/actionlint/cmd/actionlint //disabled
    go install github.com/mattn/goveralls

    if ! echo "$PATH" | grep -q "${GOPATH:-undefined}/bin\|$HOME/go/bin"; then
        echo "Go workspace's \"bin\" directory is not in PATH. Run 'export PATH=\"\$PATH:\${GOPATH:-\$HOME/go}/bin\"'."
    fi
}

kubebuilder() {
    if ! mkdir -p ${KUBEBUILDER_ASSETS}; then
      sudo mkdir -p ${KUBEBUILDER_ASSETS}
      sudo chown $(whoami) ${KUBEBUILDER_ASSETS}
    fi
    arch=$(go env GOARCH)
    ln -sf $(setup-envtest use -p path "${K8S_VERSION}" --arch="${arch}" --bin-dir="${KUBEBUILDER_ASSETS}")/* ${KUBEBUILDER_ASSETS}
    find $KUBEBUILDER_ASSETS
}

main "$@"
