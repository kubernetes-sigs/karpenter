# Adding validation to both v1 and v1beta1 APIs
# Version = 0 // v1 API 
# Version = 1 // v1beta1 API
for Version in  $(seq 0 1); do
    # Adding validation for nodepool.spec.template.spec.resources 
    yqVersion="$Version" yq eval ".spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.resources.maxProperties = 0" -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
done