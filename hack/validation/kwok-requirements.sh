# Requirements Validation 
## checking for restricted labels while filtering out well-known labels

KOWKRequirementsValidation='{"message": "label domain \"karpenter.kwok.sh\" is restricted", "rule": "self in [\"karpenter.kwok.sh/instance-cpu\", \"karpenter.kwok.sh/instance-memory\", \"karpenter.kwok.sh/instance-family\", \"karpenter.kwok.sh/instance-size\"] || !self.find(\"^([^/]+)\").endsWith(\"karpenter.kwok.sh\")"}'
# Adding validation to both v1 and v1beta1 APIs
# Version = 0 // v1 API 
# Version = 1 // v1beta1 API
for Version in {0..1}; do 
    # NodeClaim Validation:
    yq eval ".spec.versions[${Version}].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.key.x-kubernetes-validations += [${KOWKRequirementsValidation}]" -i kwok/charts/crds/karpenter.sh_nodeclaims.yaml

    # NodePool Validation: 
    yq eval ".spec.versions[${Version}].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.key.x-kubernetes-validations  += [${KOWKRequirementsValidation}]" -i kwok/charts/crds/karpenter.sh_nodepools.yaml
done