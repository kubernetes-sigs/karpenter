#!/usr/bin/env bash
# # Remove Resource Field on the spec.template.spec.resources on v1 NodePool API
yq eval 'del(.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.resources)' -i pkg/apis/crds/karpenter.sh_nodepools.yaml