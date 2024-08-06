# Updating the set of required items in our status conditions so that we support older versions of the condition
for Version in  $(seq 0 1); do 
    yqVersion="$Version" yq eval 'del(.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.status.properties.conditions.items.properties.reason.minLength)' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.status.properties.conditions.items.properties.reason.pattern = "^([A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?|)$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.status.properties.conditions.items.required = ["lastTransitionTime","status","type"]' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
done