package machine

import (
	"context"
	"fmt"

	"github.com/avast/retry-go"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/storage/names"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha1"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/utils/sets"
)

func HydrateAll(ctx context.Context, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) error {
	logging.FromContext(ctx).Debugf("hydrating machines from existing nodes owned by a provisioner")
	nodeList := &v1.NodeList{}
	if err := kubeClient.List(ctx, nodeList, client.HasLabels{v1alpha5.ProvisionerNameLabelKey}); err != nil {
		return fmt.Errorf("listing nodes, %w", err)
	}
	provisionerList := &v1alpha5.ProvisionerList{}
	if err := kubeClient.List(ctx, provisionerList); err != nil {
		return fmt.Errorf("listing provisioners, %w", err)
	}
	machineList := &v1alpha1.MachineList{}
	if err := kubeClient.List(ctx, machineList); err != nil {
		return fmt.Errorf("listing machines, %w", err)
	}
	provisionerMap := lo.SliceToMap(provisionerList.Items, func(p v1alpha5.Provisioner) (string, *v1alpha5.Provisioner) {
		return p.Name, &p
	})
	// Keep track of which machines have already been hydrated, we don't need to hydrate these
	hydratedMachines := sets.New[string](lo.Map(machineList.Items, func(m v1alpha1.Machine, _ int) string {
		return m.Status.ProviderID
	})...)
	machineNames := sets.New[string](lo.Map(machineList.Items, func(m v1alpha1.Machine, _ int) string {
		return m.Name
	})...)
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		provisioner, ok := provisionerMap[node.Labels[v1alpha5.ProvisionerNameLabelKey]]
		if !ok {
			return fmt.Errorf("provisioner not found for node '%s' with provisioner label", node.Name)
		}
		if hydratedMachines.Has(node.Spec.ProviderID) {
			continue
		}
		// Allow for multiple attempts to hydrating before failing outright
		if err := retry.Do(func() error {
			return hydrate(ctx, kubeClient, cloudProvider, node, provisioner, machineNames)
		}); err != nil {
			return fmt.Errorf("hydrating machine from node '%s', %w", node.Name, err)
		}
	}
	logging.FromContext(ctx).Debugf("finished hydrating machines from existing nodes")
	return nil
}

func hydrate(ctx context.Context, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider,
	node *v1.Node, provisioner *v1alpha5.Provisioner, machineNames sets.Set[string]) error {

	machine := v1alpha1.MachineFromNode(node)
	machine.Name = generateMachineName(machineNames, provisioner.Name) // so we know the name before creation
	machine.Spec.Kubelet = provisioner.Spec.KubeletConfiguration

	if provisioner.Spec.Provider == nil && provisioner.Spec.ProviderRef == nil {
		return fmt.Errorf("provisioner '%s' has no 'spec.provider' or 'spec.providerRef'", provisioner.Name)
	}
	if provisioner.Spec.ProviderRef != nil {
		machine.Spec.MachineTemplateRef = provisioner.Spec.ProviderRef.ToObjectReference()
	} else {
		machine.Annotations[v1alpha5.ProviderCompatabilityAnnotationKey] = v1alpha5.ProviderAnnotation(provisioner.Spec.Provider)
	}
	lo.Must0(controllerutil.SetOwnerReference(provisioner, machine, scheme.Scheme)) // shouldn't fail

	// Hydrates the machine with the correct tags at the cloud provider
	// This also updates the machine if there are any existing tags for it
	if err := cloudProvider.HydrateMachine(ctx, machine); err != nil {
		if cloudprovider.IsInstanceNotFound(err) {
			return nil
		}
		return fmt.Errorf("hydrating machine, %w", err)
	}
	statusCopy := machine.DeepCopy()
	if err := kubeClient.Create(ctx, machine); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating hydrated machine from node '%s', %w", node.Name, err)
		}
	}
	machine.Labels = lo.Assign(machine.Labels, map[string]string{
		v1alpha5.MachineNameLabelKey: machine.Name,
	})
	if err := kubeClient.Update(ctx, machine); err != nil {
		return fmt.Errorf("updating hydrated machine label for machine '%s', %w", machine.Name, err)
	}
	if err := kubeClient.Status().Patch(ctx, statusCopy, client.MergeFrom(machine)); err != nil {
		return fmt.Errorf("updating status for hydrated machine '%s', %w", machine.Name, err)
	}
	return nil
}

func generateMachineName(existingNames sets.Set[string], provisionerName string) string {
	proposed := names.SimpleNameGenerator.GenerateName(provisionerName + "-")
	for existingNames.Has(proposed) {
		proposed = names.SimpleNameGenerator.GenerateName(provisionerName + "-")
	}
	return proposed
}
