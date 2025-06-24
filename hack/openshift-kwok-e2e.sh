#!/bin/bash

set -eou pipefail

# This script requires an authenticated oc session.

# enable ko for openshift cluster
oc patch configs.imageregistry.operator.openshift.io/cluster --patch '{"spec":{"defaultRoute":true}}' --type=merge
REGISTRY_HOST=$(oc get routes --namespace openshift-image-registry default-route -o jsonpath='{.spec.host}')
oc registry login --to="${HOME}/.docker/config.json" --skip-check --registry "${REGISTRY_HOST}"
oc create clusterrolebinding authenticated-registry-viewer --clusterrole registry-viewer --group system:authenticated || true

CLEANUP=${CLEANUP:-true}
exit_handler() {
  echo "Cleaning up..."
  oc adm taint nodes --all CriticalAddonsOnly:NoSchedule- --overwrite

  if [[ "${CLEANUP}" == "true" ]]; then
    oc delete nodepools --all 
    make delete
    make uninstall-kwok
    oc delete deploy -n default --all
  fi
}

trap exit_handler EXIT

# install kwok controller
make install-kwok
# create ko namespace that holds the images built by ko
ko_namespace=ko-images
oc create namespace "${ko_namespace}" || true
# install karpenter-provider-kwok
KWOK_REPO="${REGISTRY_HOST}/${ko_namespace}" make apply-with-openshift

# tests expect all nodes to be tainted before running:
# https://github.com/kubernetes-sigs/karpenter/blob/main/test/pkg/environment/common/setup.go#L87
echo "Tainting all nodes..."
oc adm taint nodes --all CriticalAddonsOnly:NoSchedule --overwrite

# run e2e tests
make e2etests
