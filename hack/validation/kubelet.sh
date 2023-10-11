# Kubelet Validation 

# The regular expression will be adding a check for if kublet.evictionHard and kubelet.evictionSoft are percentage or a quantity value
# Adding validation for nodeclaim 
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.kubelet.properties.evictionHard.additionalProperties.pattern = "^((\d{1,2}(\.\d{1,2})?|100(\.0{1,2})?)%||(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?)$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.kubelet.properties.evictionSoft.additionalProperties.pattern = "^((\d{1,2}(\.\d{1,2})?|100(\.0{1,2})?)%||(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?)$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 

# The regular expression will be adding a check for if kublet.evictionHard and kubelet.evictionSoft are percentage or a quantity value
# Adding validation for nodepool
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.kubelet.properties.evictionHard.additionalProperties.pattern = "^((\d{1,2}(\.\d{1,2})?|100(\.0{1,2})?)%||(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?)$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.kubelet.properties.evictionSoft.additionalProperties.pattern = "^((\d{1,2}(\.\d{1,2})?|100(\.0{1,2})?)%||(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?)$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
