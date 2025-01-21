# Requirements Validation
# NodeClaim Validation:
## Qualified name for requirement keys
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.key.maxLength = 316' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.key.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
## checking for restricted labels while filtering out well-known labels
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.key.x-kubernetes-validations += [
    {"message": "label domain \"karpenter.sh\" is restricted", "rule": "self in [\"karpenter.sh/capacity-type\", \"karpenter.sh/nodepool\"] || !self.find(\"^([^/]+)\").endsWith(\"karpenter.sh\")"},
    {"message": "label \"kubernetes.io/hostname\" is restricted", "rule": "self != \"kubernetes.io/hostname\""}]' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
## operator enum values
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.operator.enum += ["In","NotIn","Exists","DoesNotExist","Gt","Lt"]' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
## Valid requirement value check
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.values.maxLength = 63' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.values.pattern = "^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml

# NodePool Validation:
## Qualified name for requirement keys
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.key.maxLength = 316' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.key.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
## checking for restricted labels while filtering out well-known labels
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.key.x-kubernetes-validations += [
    {"message": "label domain \"karpenter.sh\" is restricted", "rule": "self in [\"karpenter.sh/capacity-type\", \"karpenter.sh/nodepool\"] || !self.find(\"^([^/]+)\").endsWith(\"karpenter.sh\")"},
    {"message": "label \"karpenter.sh/nodepool\" is restricted", "rule": "self != \"karpenter.sh/nodepool\""},
    {"message": "label \"kubernetes.io/hostname\" is restricted", "rule": "self != \"kubernetes.io/hostname\""}]' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
## operator enum values
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.operator.enum  += ["In","NotIn","Exists","DoesNotExist","Gt","Lt"]' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
## Valid requirement value check
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.values.maxLength = 63' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.values.pattern  = "^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml