#!/bin/bash
set -e

ARCH=""
case "$(uname -m)" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

echo "Installing tools for ${ARCH} architecture..."

# Install kubectl
echo "Installing kubectl..."
K8S_STABLE_VERSION=$(curl -L -s https://dl.k8s.io/release/stable.txt)
curl -sSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${K8S_STABLE_VERSION}/bin/linux/${ARCH}/kubectl"
chmod +x /usr/local/bin/kubectl

# Install helm
echo "Installing helm..."
curl -sSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash > /dev/null

# Install kind
echo "Installing kind..."
KIND_LATEST_VERSION=$(curl -s "https://api.github.com/repos/kubernetes-sigs/kind/releases/latest" | grep -o '"tag_name": ".*"' | cut -d'"' -f4)
curl -sSL -o /usr/local/bin/kind "https://kind.sigs.k8s.io/dl/${KIND_LATEST_VERSION}/kind-linux-${ARCH}"
chmod +x /usr/local/bin/kind

# Install kwok
echo "Installing kwok..."
KWOK_LATEST_VERSION=$(curl -s "https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest" | grep -o '"tag_name": ".*"' | cut -d'"' -f4)
curl -sSL -o /usr/local/bin/kwokctl "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_LATEST_VERSION}/kwokctl-linux-${ARCH}"
curl -sSL -o /usr/local/bin/kwok "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_LATEST_VERSION}/kwok-linux-${ARCH}"
chmod +x /usr/local/bin/kwok /usr/local/bin/kwokctl

echo "Tools installation complete." 