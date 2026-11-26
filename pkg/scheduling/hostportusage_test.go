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

package scheduling

import (
	"fmt"
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("HostPortUsage", func() {
	Context("native sidecars", func() {
		It("tracks restartable init container ports without tracking ordinary init containers", func() {
			always := v1.ContainerRestartPolicyAlways
			pod := &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name: "app",
						Ports: []v1.ContainerPort{
							{ContainerPort: 8080, HostPort: 8080, Protocol: v1.ProtocolTCP},
							{ContainerPort: 8081, Protocol: v1.ProtocolTCP},
						},
					}},
					InitContainers: []v1.Container{
						{
							Name:  "init",
							Ports: []v1.ContainerPort{{ContainerPort: 9090, HostPort: 9090, Protocol: v1.ProtocolTCP}},
						},
						{
							Name:          "sidecar",
							RestartPolicy: &always,
							Ports: []v1.ContainerPort{
								{ContainerPort: 5353, HostPort: 5353, HostIP: "::1", Protocol: v1.ProtocolUDP},
								{ContainerPort: 5354, Protocol: v1.ProtocolUDP},
							},
						},
					},
				},
			}
			original := pod.DeepCopy()
			Expect(GetHostPorts(pod)).To(ConsistOf(
				HostPort{IP: net.IPv4zero, Port: 8080, Protocol: v1.ProtocolTCP},
				HostPort{IP: net.ParseIP("::1"), Port: 5353, Protocol: v1.ProtocolUDP},
			))
			Expect(pod).To(Equal(original))
		})
		It("rejects conflicting host ports in either scheduling order", func() {
			always := v1.ContainerRestartPolicyAlways
			sidecarPod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "sidecar-pod", Namespace: "default"},
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{{
						Name:          "sidecar",
						RestartPolicy: &always,
						Ports:         []v1.ContainerPort{{ContainerPort: 8080, HostPort: 8080, Protocol: v1.ProtocolTCP}},
					}},
				},
			}
			appPod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "default"},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:  "app",
						Ports: []v1.ContainerPort{{ContainerPort: 8080, HostPort: 8080, Protocol: v1.ProtocolTCP}},
					}},
				},
			}
			usage := NewHostPortUsage()
			usage.Add(sidecarPod, GetHostPorts(sidecarPod))
			Expect(usage.Conflicts(appPod, GetHostPorts(appPod))).To(HaveOccurred())

			usage = NewHostPortUsage()
			usage.Add(appPod, GetHostPorts(appPod))
			Expect(usage.Conflicts(sidecarPod, GetHostPorts(sidecarPod))).To(HaveOccurred())
		})
	})
	Context("entry type UT", func() {
		It("String() output", func() {
			ipVal := net.IPv4(10, 0, 0, 0)
			portVal := int32(4443)
			protocolVal := v1.ProtocolTCP
			e := HostPort{
				IP:       ipVal,
				Port:     portVal,
				Protocol: protocolVal,
			}
			Expect(e.String()).To(Equal(fmt.Sprintf("IP=%s Port=%d Proto=%s", ipVal, portVal, protocolVal)))
		})
		It("identical entries match", func() {
			ipVal := net.IPv4(10, 0, 0, 0)
			portVal := int32(4443)
			protocolVal := v1.ProtocolTCP
			e1 := HostPort{
				IP:       ipVal,
				Port:     portVal,
				Protocol: protocolVal,
			}
			e2 := e1
			Expect(e1.Matches(e2)).To(BeTrue())
			Expect(e2.Matches(e1)).To(BeTrue())
		})
		It("if any one IP has an unspecified IPv4 or IPv6 address, they match", func() {
			ipVal := net.IPv4(10, 0, 0, 0)
			portVal := int32(4443)
			protocolVal := v1.ProtocolTCP
			e1 := HostPort{
				IP:       ipVal,
				Port:     portVal,
				Protocol: protocolVal,
			}
			e2 := HostPort{
				IP:       net.IPv4zero,
				Port:     portVal,
				Protocol: protocolVal,
			}
			Expect(e1.Matches(e2)).To(BeTrue())
			Expect(e2.Matches(e1)).To(BeTrue())
			e2.IP = net.IPv6zero
			Expect(e1.Matches(e2)).To(BeTrue())
			Expect(e2.Matches(e1)).To(BeTrue())
		})
		It("mismatched protocols don't match", func() {
			ipVal := net.IPv4(10, 0, 0, 0)
			portVal := int32(4443)
			protocolVal := v1.ProtocolTCP
			e1 := HostPort{
				IP:       ipVal,
				Port:     portVal,
				Protocol: protocolVal,
			}
			e2 := e1
			e2.Protocol = v1.ProtocolSCTP
			Expect(e1.Matches(e2)).To(BeFalse())
			Expect(e2.Matches(e1)).To(BeFalse())
		})
		It("mismatched ports don't match", func() {
			ipVal := net.IPv4(10, 0, 0, 0)
			portVal := int32(4443)
			protocolVal := v1.ProtocolTCP
			e1 := HostPort{
				IP:       ipVal,
				Port:     portVal,
				Protocol: protocolVal,
			}
			e2 := e1
			e2.Port = int32(443)
			Expect(e1.Matches(e2)).To(BeFalse())
			Expect(e2.Matches(e1)).To(BeFalse())
		})
	})
})
