/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apis

import (
	_ "embed"

	"github.com/samber/lo"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/utils/functional"
)

var (
	// Builder includes all types within the apis package
	Builder = runtime.NewSchemeBuilder(
		v1alpha5.SchemeBuilder.AddToScheme,
		v1beta1.SchemeBuilder.AddToScheme,
	)
	// AddToScheme may be used to add all resources defined in the project to a Scheme
	AddToScheme = Builder.AddToScheme
	Settings    = []settings.Injectable{&settings.Settings{}}
)

//go:generate controller-gen crd:generateEmbeddedObjectMeta=true object:headerFile="../../hack/boilerplate.go.txt" paths="./..." output:crd:artifacts:config=crds
var (
	//go:embed crds/karpenter.sh_provisioners.yaml
	ProvisionerCRD []byte
	//go:embed crds/karpenter.sh_machines.yaml
	MachineCRD []byte
	//go:embed crds/karpenter.sh_nodepools.yaml
	NodePoolCRD []byte
	//go:embed crds/karpenter.sh_nodeclaims.yaml
	NodeClaimCRD []byte
	CRDs         = []*v1.CustomResourceDefinition{
		lo.Must(functional.Unmarshal[v1.CustomResourceDefinition](ProvisionerCRD)),
		lo.Must(functional.Unmarshal[v1.CustomResourceDefinition](MachineCRD)),
		lo.Must(functional.Unmarshal[v1.CustomResourceDefinition](NodePoolCRD)),
		lo.Must(functional.Unmarshal[v1.CustomResourceDefinition](NodeClaimCRD)),
	}
)
