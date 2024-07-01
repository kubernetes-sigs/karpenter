# Adding validation v1beta1 NodePool API

# Adding validation for nodepool.spec.template.spec.resources 
yq eval ".spec.versions[1].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.resources.maxProperties = 0" -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
