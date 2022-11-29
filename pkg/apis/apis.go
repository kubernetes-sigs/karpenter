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

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"knative.dev/pkg/webhook/resourcesemantics"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter-core/pkg/utils/sets"
)

const (
	Group           = "karpenter.sh"
	ExtensionsGroup = "extensions." + Group
	TestingGroup    = "testing." + Group // Exclusively used for labeling/discovery in testing
)

var (
	// Builder includes all types within the apis package
	Builder = runtime.NewSchemeBuilder(
		v1alpha5.SchemeBuilder.AddToScheme,
	)
	// AddToScheme may be used to add all resources defined in the project to a Scheme
	AddToScheme = Builder.AddToScheme
	// Resources defined in the project
	Resources = map[schema.GroupVersionKind]resourcesemantics.GenericCRD{
		v1alpha5.SchemeGroupVersion.WithKind("Provisioner"): &v1alpha5.Provisioner{},
	}
	Settings = sets.New(settings.Registration)
)

//go:generate controller-gen crd object:headerFile="../../hack/boilerplate.go.txt" paths="./..." output:crd:artifacts:config=crds
var (
	//go:embed crds/karpenter.sh_provisioners.yaml
	ProvisionerCRD []byte
	CRDs           = []*v1.CustomResourceDefinition{
		lo.Must(functional.Unmarshal[v1.CustomResourceDefinition](ProvisionerCRD)),
	}
)
