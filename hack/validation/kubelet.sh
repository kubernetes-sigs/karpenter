# Kubelet Validation 

# Adding validation to both v1 and v1beta1 APIs
# Version = 0 // v1 API 
# Version = 1 // v1beta1 API
for Version in $(seq 0 1); do 
    # The regular expression adds validation for kubelet.kubeReserved and kubelet.systemReserved values of the map are resource.Quantity
    # Quantity: https://github.com/kubernetes/apimachinery/blob/d82afe1e363acae0e8c0953b1bc230d65fdb50e2/pkg/api/resource/quantity.go#L100
    # NodeClaim Validation:
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.kubelet.properties.kubeReserved.additionalProperties.pattern = "^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.kubelet.properties.systemReserved.additionalProperties.pattern = "^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
    # NodePool Validation:
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.kubelet.properties.kubeReserved.additionalProperties.pattern = "^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.kubelet.properties.systemReserved.additionalProperties.pattern = "^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 

    # The regular expression is a validation for kubelet.evictionHard and kubelet.evictionSoft are percentage or a resource.Quantity
    # Quantity: https://github.com/kubernetes/apimachinery/blob/d82afe1e363acae0e8c0953b1bc230d65fdb50e2/pkg/api/resource/quantity.go#L100
    # NodeClaim Validation:
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.kubelet.properties.evictionHard.additionalProperties.pattern = "^((\d{1,2}(\.\d{1,2})?|100(\.0{1,2})?)%||(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?)$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.kubelet.properties.evictionSoft.additionalProperties.pattern = "^((\d{1,2}(\.\d{1,2})?|100(\.0{1,2})?)%||(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?)$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 

    # # NodePool Validation: 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.kubelet.properties.evictionHard.additionalProperties.pattern = "^((\d{1,2}(\.\d{1,2})?|100(\.0{1,2})?)%||(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?)$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.kubelet.properties.evictionSoft.additionalProperties.pattern = "^((\d{1,2}(\.\d{1,2})?|100(\.0{1,2})?)%||(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?)$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
done
