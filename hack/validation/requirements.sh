# Requirements Validation 

# Adding validation for nodeclaim 

## Qualified name for requirement keys 
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.key.maxLength = 316' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.key.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
## checking for restricted labels while filtering out well-known labels
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.key.x-kubernetes-validations += [
    {"message": "label domain \"kubernetes.io\" is restricted", "rule": "self == \"beta.kubernetes.io/instance-type\" || self == \"failure-domain.beta.kubernetes.io/region\"|| self == \"beta.kubernetes.io/os\" || self == \"beta.kubernetes.io/arch\" || self == \"failure-domain.beta.kubernetes.io/zone\" || self.startsWith(\"node.kubernetes.io/\") || self.startsWith(\"node-restriction.kubernetes.io/\") || self == \"topology.kubernetes.io/zone\" || self == \"topology.kubernetes.io/region\" || self == \"node.kubernetes.io/instance-type\" || self == \"kubernetes.io/arch\"|| self == \"kubernetes.io/os\" || self ==  \"node.kubernetes.io/windows-build\" || !self.find(\"^([^/]+)\").endsWith(\"kubernetes.io\")"},
    {"message": "label domain \"k8s.io\" is restricted", "rule": "self.startsWith(\"kops.k8s.io/\") || !self.find(\"^([^/]+)\").endsWith(\"k8s.io\")"},
    {"message": "label domain \"karpenter.sh\" is restricted", "rule": "self == \"karpenter.sh/capacity-type\"|| self == \"karpenter.sh/nodepool\" || !self.find(\"^([^/]+)\").endsWith(\"karpenter.sh\")"},
    {"message": "label \"kubernetes.io/hostname\" is restricted", "rule": "self != \"kubernetes.io/hostname\""}]' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml 
## operator enum values 
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.operator.enum += ["In","NotIn","Exists","DoesNotExist","Gt","Lt"]' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
## Vaild requirement value check  
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.values.maxLength = 63' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.requirements.items.properties.values.pattern = "^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$" ' -i pkg/apis/crds/karpenter.sh_nodeclaims.yaml

# Adding validation for nodepool

## Qualified name for requirement keys 
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.key.maxLength = 316' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.key.pattern = "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(\/))?([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$"' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
## checking for restricted labels while filtering out well-known labels
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.key.x-kubernetes-validations  += [
    {"message": "label domain \"kubernetes.io\" is restricted", "rule": "self == \"beta.kubernetes.io/instance-type\" || self == \"failure-domain.beta.kubernetes.io/region\"|| self == \"beta.kubernetes.io/os\" || self == \"beta.kubernetes.io/arch\" || self == \"failure-domain.beta.kubernetes.io/zone\" || self.startsWith(\"node.kubernetes.io/\") || self.startsWith(\"node-restriction.kubernetes.io/\") || self == \"topology.kubernetes.io/zone\" || self == \"topology.kubernetes.io/region\" || self == \"node.kubernetes.io/instance-type\" || self == \"kubernetes.io/arch\"|| self == \"kubernetes.io/os\" || self ==  \"node.kubernetes.io/windows-build\" || !self.find(\"^([^/]+)\").endsWith(\"kubernetes.io\")"},
    {"message": "label domain \"k8s.io\" is restricted", "rule": "self.startsWith(\"kops.k8s.io/\") || !self.find(\"^([^/]+)\").endsWith(\"k8s.io\")"},
    {"message": "label domain \"karpenter.sh\" is restricted", "rule": "self == \"karpenter.sh/capacity-type\"|| self == \"karpenter.sh/nodepool\" || !self.find(\"^([^/]+)\").endsWith(\"karpenter.sh\")"},
    {"message": "label \"karpenter.sh/nodepool\" is restricted", "rule": "self != \"karpenter.sh/nodepool\""},
    {"message": "label \"kubernetes.io/hostname\" is restricted", "rule": "self != \"kubernetes.io/hostname\""}]' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
## operator enum values 
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.operator.enum  += ["In","NotIn","Exists","DoesNotExist","Gt","Lt"]' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
## Vaild requirement value check  
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.values.maxLength = 63' -i pkg/apis/crds/karpenter.sh_nodepools.yaml
yq eval '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.template.properties.spec.properties.requirements.items.properties.values.pattern  = "^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$" ' -i pkg/apis/crds/karpenter.sh_nodepools.yaml