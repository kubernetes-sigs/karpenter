# Taints Validation 

# Adding validation to both v1 and v1beta1 APIs
# Version = 0 // v1 API 
# Version = 1 // v1beta1 API
for Version in  $(seq 0 1); do 
    # NodeClaim Validation:
    ## Taint
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.taints.items.properties.key.minLength = 1' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.taints.items.properties.key.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.taints.items.properties.value.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml  
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.taints.items.properties.effect.enum += ["NoSchedule","PreferNoSchedule","NoExecute"]' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml  

    ## Startup-Taint
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.startupTaints.items.properties.key.minLength = 1' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.startupTaints.items.properties.key.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.startupTaints.items.properties.value.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.startupTaints.items.properties.effect.enum += ["NoSchedule","PreferNoSchedule","NoExecute"]' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml  

    # Nodepool Validation:
    ## Taint
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.taints.items.properties.key.minLength = 1' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.taints.items.properties.key.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.taints.items.properties.value.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml  
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.taints.items.properties.effect.enum += ["NoSchedule","PreferNoSchedule","NoExecute"]' -i pkg/apis/crds/karpenter.sh_nodepools.yaml  

    ## Startup-Taint
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.startupTaints.items.properties.key.minLength = 1' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.startupTaints.items.properties.key.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.startupTaints.items.properties.value.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.startupTaints.items.properties.effect.enum += ["NoSchedule","PreferNoSchedule","NoExecute"]' -i pkg/apis/crds/karpenter.sh_nodepools.yaml  
done

