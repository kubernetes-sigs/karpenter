#!/usr/bin/env bash
# # Remove Resource Field on the spec.template.spec.resources on v1 NodePool API
yq eval 'del(.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.resources)' -i pkg/apis/crds/karpenter.sh_nodepools.yaml

# Add the conversion stanza to the CRD spec to enable conversion via webhook
yq eval '.spec.conversion = {"strategy": "Webhook", "webhook": {"conversionReviewVersions": ["v1beta1", "v1"], "clientConfig": {"service": {"name": "karpenter", "namespace": "kube-system", "port": 8443}}}}' -i kwok/apis/crds/karpenter.kwok.sh_kwoknodeclasses.yaml
yq eval '.spec.conversion = {"strategy": "Webhook", "webhook": {"conversionReviewVersions": ["v1beta1", "v1"], "clientConfig": {"service": {"name": "karpenter", "namespace": "kube-system", "port": 8443}}}}' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
yq eval '.spec.conversion = {"strategy": "Webhook", "webhook": {"conversionReviewVersions": ["v1beta1", "v1"], "clientConfig": {"service": {"name": "karpenter", "namespace": "kube-system", "port": 8443}}}}' -i pkg/apis/crds/karpenter.sh_nodepools.yaml