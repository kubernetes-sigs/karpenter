package ec2

import "github.com/aws/aws-sdk-go-v2/service/ec2/types"

type CreateFleetOutput struct {
	FleetId   *string
	Errors    []types.CreateFleetError
	Instances []types.CreateFleetInstance
}
