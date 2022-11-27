#!/usr/bin/env bash
set -euo pipefail

config(){
  GITHUB_ACCOUNT="spring1843"
  RELEASE_REPO=${RELEASE_REPO:-ghcr.io/${GITHUB_ACCOUNT}/}
}

publishHelmChart() {
    CHART_NAME=$1
    HELM_CHART_VERSION=$2
    HELM_CHART_FILE_NAME="karpenter-core-crd-${HELM_CHART_VERSION}.tgz"

    cd charts
    helm lint "${CHART_NAME}"
    helm package "${CHART_NAME}" --version $HELM_CHART_VERSION
    helm push "${HELM_CHART_FILE_NAME}" "oci://${RELEASE_REPO}/karpenter-core"
    rm "${HELM_CHART_FILE_NAME}"
    cd ..
}
