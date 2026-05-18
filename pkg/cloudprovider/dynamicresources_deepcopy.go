/*
Copyright The Kubernetes Authors.

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

package cloudprovider

import (
	resourcev1 "k8s.io/api/resource/v1"
)

func (in *ResourceSliceTemplate) DeepCopyInto(out *ResourceSliceTemplate) {
	*out = *in
	if in.Devices != nil {
		out.Devices = make([]Device, len(in.Devices))
		for i := range in.Devices {
			in.Devices[i].DeepCopyInto(&out.Devices[i])
		}
	}
}

func (in *ResourceSliceTemplate) DeepCopy() *ResourceSliceTemplate {
	if in == nil {
		return nil
	}
	out := new(ResourceSliceTemplate)
	in.DeepCopyInto(out)
	return out
}

func (in *Device) DeepCopyInto(out *Device) {
	*out = *in
	if in.Attributes != nil {
		out.Attributes = make(map[resourcev1.QualifiedName]resourcev1.DeviceAttribute, len(in.Attributes))
		for key, val := range in.Attributes {
			out.Attributes[key] = *val.DeepCopy()
		}
	}
}

func (in *Device) DeepCopy() *Device {
	if in == nil {
		return nil
	}
	out := new(Device)
	in.DeepCopyInto(out)
	return out
}

func (in *AttributeBinding) DeepCopyInto(out *AttributeBinding) {
	*out = *in
	if in.Devices != nil {
		out.Devices = make([]DeviceID, len(in.Devices))
		copy(out.Devices, in.Devices)
	}
}

func (in *AttributeBinding) DeepCopy() *AttributeBinding {
	if in == nil {
		return nil
	}
	out := new(AttributeBinding)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto for ResourcePool is a simple value copy since unique.Handle is immutable.
func (in *ResourcePool) DeepCopyInto(out *ResourcePool) {
	*out = *in
}

func (in *ResourcePool) DeepCopy() *ResourcePool {
	if in == nil {
		return nil
	}
	out := new(ResourcePool)
	*out = *in
	return out
}

// DeepCopyInto for DeviceID is a simple value copy since all fields are unique.Handle (immutable).
func (in *DeviceID) DeepCopyInto(out *DeviceID) {
	*out = *in
}

func (in *DeviceID) DeepCopy() *DeviceID {
	if in == nil {
		return nil
	}
	out := new(DeviceID)
	*out = *in
	return out
}
