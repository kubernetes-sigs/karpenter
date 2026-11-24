package types

type InstanceType string

type FleetLaunchTemplateOverrides struct {
	AvailabilityZone *string
	SubnetId         *string
	InstanceType     InstanceType
}

func (o FleetLaunchTemplateOverrides) Zone() string { return "" }

type LaunchTemplateAndOverridesResponse struct {
	Overrides *FleetLaunchTemplateOverrides
}

type CreateFleetError struct {
	ErrorCode                  *string
	LaunchTemplateAndOverrides *LaunchTemplateAndOverridesResponse
}

type CreateFleetInstance struct {
	InstanceIds                []string
	LaunchTemplateAndOverrides *LaunchTemplateAndOverridesResponse
}
