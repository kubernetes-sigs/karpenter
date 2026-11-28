# Labels Validation 

# NodePool Validation:
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.metadata.properties.labels.maxProperties = 100' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
# Vaild requirement value check
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.metadata.properties.labels.additionalProperties.maxLength = 63' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.metadata.properties.labels.additionalProperties.pattern  = "^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml