package a

import (
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"
)

func chain(err types.CreateFleetError) types.InstanceType {
	return err.LaunchTemplateAndOverrides.Overrides.InstanceType // want `err.LaunchTemplateAndOverrides is an optional AWS SDK pointer`
}

func explicitStar(inst types.CreateFleetInstance) string {
	if inst.LaunchTemplateAndOverrides == nil {
		return ""
	}
	return *inst.LaunchTemplateAndOverrides.Overrides.AvailabilityZone // want `inst.LaunchTemplateAndOverrides.Overrides is an optional AWS SDK pointer`
}

func outputField(out *ec2.CreateFleetOutput) string {
	return *out.FleetId // want `out.FleetId is an optional AWS SDK pointer`
}

func valueMethod(err types.CreateFleetError) string {
	if err.LaunchTemplateAndOverrides == nil {
		return ""
	}
	return err.LaunchTemplateAndOverrides.Overrides.Zone() // want `err.LaunchTemplateAndOverrides.Overrides is an optional AWS SDK pointer`
}

func guarded(err types.CreateFleetError) types.InstanceType {
	if err.LaunchTemplateAndOverrides != nil && err.LaunchTemplateAndOverrides.Overrides != nil {
		return err.LaunchTemplateAndOverrides.Overrides.InstanceType
	}
	return ""
}

func earlyReturn(inst types.CreateFleetInstance) string {
	if inst.LaunchTemplateAndOverrides == nil || inst.LaunchTemplateAndOverrides.Overrides == nil {
		return ""
	}
	return lo.FromPtr(inst.LaunchTemplateAndOverrides.Overrides.SubnetId)
}

func fromPtr(err types.CreateFleetError) string {
	return lo.FromPtr(lo.FromPtr(lo.FromPtr(err.LaunchTemplateAndOverrides).Overrides).AvailabilityZone)
}

func passPointer(err types.CreateFleetError) *types.LaunchTemplateAndOverridesResponse {
	return err.LaunchTemplateAndOverrides
}

type local struct{ next *local }

func notSDK(l local) *local {
	return l.next.next
}

func guardedStar(inst types.CreateFleetInstance) string {
	if inst.LaunchTemplateAndOverrides == nil || inst.LaunchTemplateAndOverrides.Overrides == nil {
		return ""
	}
	return *inst.LaunchTemplateAndOverrides.Overrides.AvailabilityZone // want `inst.LaunchTemplateAndOverrides.Overrides.AvailabilityZone is an optional AWS SDK pointer`
}
