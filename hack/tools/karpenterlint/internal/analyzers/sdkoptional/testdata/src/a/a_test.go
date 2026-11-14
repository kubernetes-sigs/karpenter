package a

import "github.com/aws/aws-sdk-go-v2/service/ec2/types"

func testHelper(err types.CreateFleetError) types.InstanceType {
	return err.LaunchTemplateAndOverrides.Overrides.InstanceType
}
