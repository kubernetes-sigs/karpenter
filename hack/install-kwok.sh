#!/usr/bin/env bash
# This script uses kustomize to generate the manifests of the latest 
# version of kwok, and will then either apply/delete the resources 
# depending on the UNINSTALL_KWOK environment variable. Set this to 
# true if you want to delete the generated objects.

# This will create 1 partition (should be expanded to multiple once https://github.com/kubernetes-sigs/kwok/issues/1122 is fixed). 
# The more partitions, the higher number of nodes that can be managed by kwok.
# Note: Beware that when using kind clusters, the total compute of nodes is limited, so 
# the higher number of partitions, the less effective each partition might be, since 
# the controller pods could be competing for limited CPU.

set -euo pipefail

# get the latest version
KWOK_REPO=kubernetes-sigs/kwok
KWOK_RELEASE=v0.5.2
# make a base directory for multi-base kustomization
HOME_DIR=$(mktemp -d)
BASE=${HOME_DIR}/base
mkdir ${BASE}

# make the alphabet, so that we can set be flexible to the number of allowed partitions (inferred max at 26)
alphabet=( {a..z} )
# set the number of partitions to 1. Currently only one partition is supported. 
num_partitions=1

# allow it to schedule to critical addons, but not schedule onto kwok nodes.
cat <<EOF > "${BASE}/tolerate-all.yaml"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kwok-controller
  namespace: kube-system
spec:
  template:
    spec:
      tolerations:
      - operator: "Equal"
        key: CriticalAddonsOnly 
        effect: NoSchedule
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: kwok.x-k8s.io/node
                operator: DoesNotExist
EOF

# TODO: Simplify the kustomize to only use one copy of the RBAC that all
# controllers can use.  
cat <<EOF > "${BASE}/kustomization.yaml"
  apiVersion: kustomize.config.k8s.io/v1beta1
  kind: Kustomization
  images:
  - name: registry.k8s.io/kwok/kwok
    newTag: "${KWOK_RELEASE}"
  resources:
  - "https://github.com/${KWOK_REPO}/kustomize/kwok?ref=${KWOK_RELEASE}"
  patches:
  - path: tolerate-all.yaml
EOF

# Create num_partitions
for ((i=0; i<num_partitions; i++))
do
  SUB_LET_DIR=$HOME_DIR/${alphabet[i]}
  mkdir ${SUB_LET_DIR}

  cat <<EOF > "${SUB_LET_DIR}/patch.yaml"
  - op: replace
    path: /spec/template/spec/containers/0/args/2
    value: --manage-nodes-with-label-selector=kwok-partition=${alphabet[i]}
EOF

cat <<EOF > "${SUB_LET_DIR}/kustomization.yaml"
  apiVersion: kustomize.config.k8s.io/v1beta1
  kind: Kustomization
  images:
  - name: registry.k8s.io/kwok/kwok
    newTag: "${KWOK_RELEASE}"
  resources:
  - ./../base
  nameSuffix: -${alphabet[i]}
  patches:
    - path: ${SUB_LET_DIR}/patch.yaml
      target:
        group: apps
        version: v1
        kind: Deployment
        name: kwok-controller
EOF

done

cat <<EOF > "${HOME_DIR}/kustomization.yaml"
resources:
EOF

# Create num_partitions
for ((i=0; i<num_partitions; i++))
do
  echo " - ./${alphabet[i]}" >> "${HOME_DIR}/kustomization.yaml"
done

kubectl kustomize "${HOME_DIR}" > "${HOME_DIR}/kwok.yaml"

# v0.4.0 added in stage CRDs which are necessary for pod/node initialization
crdURL="https://github.com/${KWOK_REPO}/releases/download/${KWOK_RELEASE}/stage-fast.yaml"

# Set UNINSTALL_KWOK=true if you want to uninstall.
if [[ ${UNINSTALL_KWOK} = "true" ]]
then
  kubectl delete -f ${HOME_DIR}/kwok.yaml
  kubectl delete -f ${crdURL}
else
  kubectl apply -f ${HOME_DIR}/kwok.yaml
  kubectl apply -f ${crdURL}
fi

