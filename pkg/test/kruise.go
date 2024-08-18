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

package test

import (
	"fmt"

	"github.com/awslabs/operatorpkg/object"
	"github.com/imdario/mergo"
	kruise "github.com/openkruise/kruise/apis/apps/v1alpha1"
	"github.com/samber/lo"

	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DaemonSetOptions customizes a DaemonSet.
type KruiseDaemonSetOptions struct {
	metav1.ObjectMeta
	Selector   map[string]string
	PodOptions PodOptions
}

// KruiseDaemonSet creates a test pod with defaults that can be overridden by DaemonSetOptions.
// Overrides are applied in order, with a last write wins semantic.
func KruiseDaemonSet(overrides ...DaemonSetOptions) *kruise.DaemonSet {
	options := DaemonSetOptions{}
	for _, opts := range overrides {
		if err := mergo.Merge(&options, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("Failed to merge daemonset options: %s", err))
		}
	}
	if options.Name == "" {
		options.Name = RandomName()
	}
	if options.Namespace == "" {
		options.Namespace = "default"
	}
	if options.Selector == nil {
		options.Selector = map[string]string{"app": options.Name}
	}
	return &kruise.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: options.Name, Namespace: options.Namespace},
		Spec: kruise.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: options.Selector},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: options.Selector},
				Spec:       Pod(options.PodOptions).Spec,
			},
		},
	}
}

func KruiseStatefulSet(overrides ...StatefulSetOptions) *kruise.StatefulSet {
	options := StatefulSetOptions{}
	for _, opts := range overrides {
		if err := mergo.Merge(&options, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("Failed to merge deployment options: %s", err))
		}
	}

	objectMeta := NamespacedObjectMeta(options.ObjectMeta)

	if options.PodOptions.Image == "" {
		options.PodOptions.Image = "public.ecr.aws/eks-distro/kubernetes/pause:3.2"
	}
	if options.PodOptions.Labels == nil {
		options.PodOptions.Labels = map[string]string{
			"app": objectMeta.Name,
		}
	}
	pod := Pod(options.PodOptions)
	return &kruise.StatefulSet{
		ObjectMeta: objectMeta,
		Spec: kruise.StatefulSetSpec{
			Replicas: lo.ToPtr(options.Replicas),
			Selector: &metav1.LabelSelector{MatchLabels: options.PodOptions.Labels},
			Template: v1.PodTemplateSpec{
				ObjectMeta: ObjectMeta(options.PodOptions.ObjectMeta),
				Spec:       pod.Spec,
			},
		},
	}
}

var KruiseDaemonSetCRD = []byte(`
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  annotations:
    controller-gen.kubebuilder.io/version: v0.14.0
  name: daemonsets.apps.kruise.io
spec:
  group: apps.kruise.io
  names:
    kind: DaemonSet
    listKind: DaemonSetList
    plural: daemonsets
    shortNames:
    - daemon
    - ads
    singular: daemonset
  scope: Namespaced
  versions:
  - additionalPrinterColumns:
    - description: The desired number of pods.
      jsonPath: .status.desiredNumberScheduled
      name: DESIRED
      type: integer
    - description: The current number of pods.
      jsonPath: .status.currentNumberScheduled
      name: CURRENT
      type: integer
    - description: The ready number of pods.
      jsonPath: .status.numberReady
      name: READY
      type: integer
    - description: The updated number of pods.
      jsonPath: .status.updatedNumberScheduled
      name: UP-TO-DATE
      type: integer
    - description: The updated number of pods.
      jsonPath: .status.numberAvailable
      name: AVAILABLE
      type: integer
    - description: CreationTimestamp is a timestamp representing the server time when
        this object was created. It is not guaranteed to be set in happens-before
        order across separate operations. Clients may not set this value. It is represented
        in RFC3339 form and is in UTC.
      jsonPath: .metadata.creationTimestamp
      name: AGE
      type: date
    - description: The containers of currently  daemonset.
      jsonPath: .spec.template.spec.containers[*].name
      name: CONTAINERS
      priority: 1
      type: string
    - description: The images of currently advanced daemonset.
      jsonPath: .spec.template.spec.containers[*].image
      name: IMAGES
      priority: 1
      type: string
    name: v1alpha1
    schema:
      openAPIV3Schema:
        description: DaemonSet is the Schema for the daemonsets API
        properties:
          apiVersion:
            description: |-
              APIVersion defines the versioned schema of this representation of an object.
              Servers should convert recognized schemas to the latest internal value, and
              may reject unrecognized values.
              More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
            type: string
          kind:
            description: |-
              Kind is a string value representing the REST resource this object represents.
              Servers may infer this from the endpoint the client submits requests to.
              Cannot be updated.
              In CamelCase.
              More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
            type: string
          metadata:
            type: object
          spec:
            description: DaemonSetSpec defines the desired state of DaemonSet
            properties:
              burstReplicas:
                anyOf:
                - type: integer
                - type: string
                description: |-
                  BurstReplicas is a rate limiter for booting pods on a lot of pods.
                  The default value is 250
                x-kubernetes-int-or-string: true
              lifecycle:
                description: |-
                  Lifecycle defines the lifecycle hooks for Pods pre-delete, in-place update.
                  Currently, we only support pre-delete hook for Advanced DaemonSet.
                properties:
                  inPlaceUpdate:
                    description: InPlaceUpdate is the hook before Pod to update and
                      after Pod has been updated.
                    properties:
                      finalizersHandler:
                        items:
                          type: string
                        type: array
                      labelsHandler:
                        additionalProperties:
                          type: string
                        type: object
                      markPodNotReady:
                        description: |-
                          MarkPodNotReady = true means:
                          - Pod will be set to 'NotReady' at preparingDelete/preparingUpdate state.
                          - Pod will be restored to 'Ready' at Updated state if it was set to 'NotReady' at preparingUpdate state.
                          Currently, MarkPodNotReady only takes effect on InPlaceUpdate & PreDelete hook.
                          Default to false.
                        type: boolean
                    type: object
                  preDelete:
                    description: PreDelete is the hook before Pod to be deleted.
                    properties:
                      finalizersHandler:
                        items:
                          type: string
                        type: array
                      labelsHandler:
                        additionalProperties:
                          type: string
                        type: object
                      markPodNotReady:
                        description: |-
                          MarkPodNotReady = true means:
                          - Pod will be set to 'NotReady' at preparingDelete/preparingUpdate state.
                          - Pod will be restored to 'Ready' at Updated state if it was set to 'NotReady' at preparingUpdate state.
                          Currently, MarkPodNotReady only takes effect on InPlaceUpdate & PreDelete hook.
                          Default to false.
                        type: boolean
                    type: object
                  preNormal:
                    description: PreNormal is the hook after Pod to be created and
                      ready to be Normal.
                    properties:
                      finalizersHandler:
                        items:
                          type: string
                        type: array
                      labelsHandler:
                        additionalProperties:
                          type: string
                        type: object
                      markPodNotReady:
                        description: |-
                          MarkPodNotReady = true means:
                          - Pod will be set to 'NotReady' at preparingDelete/preparingUpdate state.
                          - Pod will be restored to 'Ready' at Updated state if it was set to 'NotReady' at preparingUpdate state.
                          Currently, MarkPodNotReady only takes effect on InPlaceUpdate & PreDelete hook.
                          Default to false.
                        type: boolean
                    type: object
                type: object
              minReadySeconds:
                description: |-
                  The minimum number of seconds for which a newly created DaemonSet pod should
                  be ready without any of its container crashing, for it to be considered
                  available. Defaults to 0 (pod will be considered available as soon as it
                  is ready).
                format: int32
                type: integer
              revisionHistoryLimit:
                description: |-
                  The number of old history to retain to allow rollback.
                  This is a pointer to distinguish between explicit zero and not specified.
                  Defaults to 10.
                format: int32
                type: integer
              selector:
                description: |-
                  A label query over pods that are managed by the daemon set.
                  Must match in order to be controlled.
                  It must match the pod template's labels.
                  More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors
                properties:
                  matchExpressions:
                    description: matchExpressions is a list of label selector requirements.
                      The requirements are ANDed.
                    items:
                      description: |-
                        A label selector requirement is a selector that contains values, a key, and an operator that
                        relates the key and values.
                      properties:
                        key:
                          description: key is the label key that the selector applies
                            to.
                          type: string
                        operator:
                          description: |-
                            operator represents a key's relationship to a set of values.
                            Valid operators are In, NotIn, Exists and DoesNotExist.
                          type: string
                        values:
                          description: |-
                            values is an array of string values. If the operator is In or NotIn,
                            the values array must be non-empty. If the operator is Exists or DoesNotExist,
                            the values array must be empty. This array is replaced during a strategic
                            merge patch.
                          items:
                            type: string
                          type: array
                      required:
                      - key
                      - operator
                      type: object
                    type: array
                  matchLabels:
                    additionalProperties:
                      type: string
                    description: |-
                      matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels
                      map is equivalent to an element of matchExpressions, whose key field is "key", the
                      operator is "In", and the values array contains only "value". The requirements are ANDed.
                    type: object
                type: object
                x-kubernetes-map-type: atomic
              template:
                description: |-
                  An object that describes the pod that will be created.
                  The DaemonSet will create exactly one copy of this pod on every node
                  that matches the template's node selector (or on every node if no node
                  selector is specified).
                  More info: https://kubernetes.io/docs/concepts/workloads/controllers/replicationcontroller#pod-template
                x-kubernetes-preserve-unknown-fields: true
              updateStrategy:
                description: An update strategy to replace existing DaemonSet pods
                  with new pods.
                properties:
                  rollingUpdate:
                    description: Rolling update config params. Present only if type
                      = "RollingUpdate".
                    properties:
                      maxSurge:
                        anyOf:
                        - type: integer
                        - type: string
                        description: |-
                          The maximum number of nodes with an existing available DaemonSet pod that
                          can have an updated DaemonSet pod during during an update.
                          Value can be an absolute number (ex: 5) or a percentage of desired pods (ex: 10%).
                          This can not be 0 if MaxUnavailable is 0.
                          Absolute number is calculated from percentage by rounding up to a minimum of 1.
                          Default value is 0.
                          Example: when this is set to 30%, at most 30% of the total number of nodes
                          that should be running the daemon pod (i.e. status.desiredNumberScheduled)
                          can have their a new pod created before the old pod is marked as deleted.
                          The update starts by launching new pods on 30% of nodes. Once an updated
                          pod is available (Ready for at least minReadySeconds) the old DaemonSet pod
                          on that node is marked deleted. If the old pod becomes unavailable for any
                          reason (Ready transitions to false, is evicted, or is drained) an updated
                          pod is immediately created on that node without considering surge limits.
                          Allowing surge implies the possibility that the resources consumed by the
                          daemonset on any given node can double if the readiness check fails, and
                          so resource intensive daemonsets should take into account that they may
                          cause evictions during disruption.
                          This is beta field and enabled/disabled by DaemonSetUpdateSurge feature gate.
                        x-kubernetes-int-or-string: true
                      maxUnavailable:
                        anyOf:
                        - type: integer
                        - type: string
                        description: |-
                          The maximum number of DaemonSet pods that can be unavailable during the
                          update. Value can be an absolute number (ex: 5) or a percentage of total
                          number of DaemonSet pods at the start of the update (ex: 10%). Absolute
                          number is calculated from percentage by rounding up.
                          This cannot be 0 if MaxSurge is 0
                          Default value is 1.
                          Example: when this is set to 30%, at most 30% of the total number of nodes
                          that should be running the daemon pod (i.e. status.desiredNumberScheduled)
                          can have their pods stopped for an update at any given time. The update
                          starts by stopping at most 30% of those DaemonSet pods and then brings
                          up new DaemonSet pods in their place. Once the new pods are available,
                          it then proceeds onto other DaemonSet pods, thus ensuring that at least
                          70% of original number of DaemonSet pods are available at all times during
                          the update.
                        x-kubernetes-int-or-string: true
                      partition:
                        description: |-
                          The number of DaemonSet pods remained to be old version.
                          Default value is 0.
                          Maximum value is status.DesiredNumberScheduled, which means no pod will be updated.
                        format: int32
                        type: integer
                      paused:
                        description: |-
                          Indicates that the daemon set is paused and will not be processed by the
                          daemon set controller.
                        type: boolean
                      rollingUpdateType:
                        description: Type is to specify which kind of rollingUpdate.
                        type: string
                      selector:
                        description: |-
                          A label query over nodes that are managed by the daemon set RollingUpdate.
                          Must match in order to be controlled.
                          It must match the node's labels.
                        properties:
                          matchExpressions:
                            description: matchExpressions is a list of label selector
                              requirements. The requirements are ANDed.
                            items:
                              description: |-
                                A label selector requirement is a selector that contains values, a key, and an operator that
                                relates the key and values.
                              properties:
                                key:
                                  description: key is the label key that the selector
                                    applies to.
                                  type: string
                                operator:
                                  description: |-
                                    operator represents a key's relationship to a set of values.
                                    Valid operators are In, NotIn, Exists and DoesNotExist.
                                  type: string
                                values:
                                  description: |-
                                    values is an array of string values. If the operator is In or NotIn,
                                    the values array must be non-empty. If the operator is Exists or DoesNotExist,
                                    the values array must be empty. This array is replaced during a strategic
                                    merge patch.
                                  items:
                                    type: string
                                  type: array
                              required:
                              - key
                              - operator
                              type: object
                            type: array
                          matchLabels:
                            additionalProperties:
                              type: string
                            description: |-
                              matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels
                              map is equivalent to an element of matchExpressions, whose key field is "key", the
                              operator is "In", and the values array contains only "value". The requirements are ANDed.
                            type: object
                        type: object
                        x-kubernetes-map-type: atomic
                    type: object
                  type:
                    description: Type of daemon set update. Can be "RollingUpdate"
                      or "OnDelete". Default is RollingUpdate.
                    type: string
                type: object
            required:
            - selector
            - template
            type: object
          status:
            description: DaemonSetStatus defines the observed state of DaemonSet
            properties:
              collisionCount:
                description: |-
                  Count of hash collisions for the DaemonSet. The DaemonSet controller
                  uses this field as a collision avoidance mechanism when it needs to
                  create the name for the newest ControllerRevision.
                format: int32
                type: integer
              conditions:
                description: Represents the latest available observations of a DaemonSet's
                  current state.
                items:
                  description: DaemonSetCondition describes the state of a DaemonSet
                    at a certain point.
                  properties:
                    lastTransitionTime:
                      description: Last time the condition transitioned from one status
                        to another.
                      format: date-time
                      type: string
                    message:
                      description: A human readable message indicating details about
                        the transition.
                      type: string
                    reason:
                      description: The reason for the condition's last transition.
                      type: string
                    status:
                      description: Status of the condition, one of True, False, Unknown.
                      type: string
                    type:
                      description: Type of DaemonSet condition.
                      type: string
                  required:
                  - status
                  - type
                  type: object
                type: array
              currentNumberScheduled:
                description: |-
                  The number of nodes that are running at least 1
                  daemon pod and are supposed to run the daemon pod.
                  More info: https://kubernetes.io/docs/concepts/workloads/controllers/daemonset/
                format: int32
                type: integer
              daemonSetHash:
                description: DaemonSetHash is the controller-revision-hash, which
                  represents the latest version of the DaemonSet.
                type: string
              desiredNumberScheduled:
                description: |-
                  The total number of nodes that should be running the daemon
                  pod (including nodes correctly running the daemon pod).
                  More info: https://kubernetes.io/docs/concepts/workloads/controllers/daemonset/
                format: int32
                type: integer
              numberAvailable:
                description: |-
                  The number of nodes that should be running the
                  daemon pod and have one or more of the daemon pod running and
                  available (ready for at least spec.minReadySeconds)
                format: int32
                type: integer
              numberMisscheduled:
                description: |-
                  The number of nodes that are running the daemon pod, but are
                  not supposed to run the daemon pod.
                  More info: https://kubernetes.io/docs/concepts/workloads/controllers/daemonset/
                format: int32
                type: integer
              numberReady:
                description: |-
                  The number of nodes that should be running the daemon pod and have one
                  or more of the daemon pod running and ready.
                format: int32
                type: integer
              numberUnavailable:
                description: |-
                  The number of nodes that should be running the
                  daemon pod and have none of the daemon pod running and available
                  (ready for at least spec.minReadySeconds)
                format: int32
                type: integer
              observedGeneration:
                description: The most recent generation observed by the daemon set
                  controller.
                format: int64
                type: integer
              updatedNumberScheduled:
                description: The total number of nodes that are running updated daemon
                  pod
                format: int32
                type: integer
            required:
            - currentNumberScheduled
            - daemonSetHash
            - desiredNumberScheduled
            - numberMisscheduled
            - numberReady
            - updatedNumberScheduled
            type: object
        type: object
    served: true
    storage: true
    subresources:
      status: {}
`)

var KruiseStatefulSetCRD = []byte(`
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  annotations:
    controller-gen.kubebuilder.io/version: v0.14.0
  name: statefulsets.apps.kruise.io
spec:
  group: apps.kruise.io
  names:
    kind: StatefulSet
    listKind: StatefulSetList
    plural: statefulsets
    shortNames:
    - sts
    - asts
    singular: statefulset
  scope: Namespaced
  versions:
  - additionalPrinterColumns:
    - description: The desired number of pods.
      jsonPath: .spec.replicas
      name: DESIRED
      type: integer
    - description: The number of currently all pods.
      jsonPath: .status.replicas
      name: CURRENT
      type: integer
    - description: The number of pods updated.
      jsonPath: .status.updatedReplicas
      name: UPDATED
      type: integer
    - description: The number of pods ready.
      jsonPath: .status.readyReplicas
      name: READY
      type: integer
    - description: CreationTimestamp is a timestamp representing the server time when
        this object was created. It is not guaranteed to be set in happens-before
        order across separate operations. Clients may not set this value. It is represented
        in RFC3339 form and is in UTC.
      jsonPath: .metadata.creationTimestamp
      name: AGE
      type: date
    - description: The containers of currently advanced statefulset.
      jsonPath: .spec.template.spec.containers[*].name
      name: CONTAINERS
      priority: 1
      type: string
    - description: The images of currently advanced statefulset.
      jsonPath: .spec.template.spec.containers[*].image
      name: IMAGES
      priority: 1
      type: string
    name: v1alpha1
    schema:
      openAPIV3Schema:
        description: StatefulSet is the Schema for the statefulsets API
        properties:
          apiVersion:
            description: |-
              APIVersion defines the versioned schema of this representation of an object.
              Servers should convert recognized schemas to the latest internal value, and
              may reject unrecognized values.
              More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
            type: string
          kind:
            description: |-
              Kind is a string value representing the REST resource this object represents.
              Servers may infer this from the endpoint the client submits requests to.
              Cannot be updated.
              In CamelCase.
              More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
            type: string
          metadata:
            type: object
          spec:
            description: StatefulSetSpec defines the desired state of StatefulSet
            properties:
              podManagementPolicy:
                description: |-
                  podManagementPolicy controls how pods are created during initial scale up,
                  when replacing pods on nodes, or when scaling down. The default policy is
                  'OrderedReady', where pods are created in increasing order (pod-0, then
                  pod-1, etc) and the controller will wait until each pod is ready before
                  continuing. When scaling down, the pods are removed in the opposite order.
                  The alternative policy is 'Parallel' which will create pods in parallel
                  to match the desired scale without waiting, and on scale down will delete
                  all pods at once.
                type: string
              replicas:
                description: |-
                  replicas is the desired number of replicas of the given Template.
                  These are replicas in the sense that they are instantiations of the
                  same Template, but individual replicas also have a consistent identity.
                  If unspecified, defaults to 1.
                  TODO: Consider a rename of this field.
                format: int32
                type: integer
              revisionHistoryLimit:
                description: |-
                  revisionHistoryLimit is the maximum number of revisions that will
                  be maintained in the StatefulSet's revision history. The revision history
                  consists of all revisions not represented by a currently applied
                  StatefulSetSpec version. The default value is 10.
                format: int32
                type: integer
              selector:
                description: |-
                  selector is a label query over pods that should match the replica count.
                  It must match the pod template's labels.
                  More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors
                properties:
                  matchExpressions:
                    description: matchExpressions is a list of label selector requirements.
                      The requirements are ANDed.
                    items:
                      description: |-
                        A label selector requirement is a selector that contains values, a key, and an operator that
                        relates the key and values.
                      properties:
                        key:
                          description: key is the label key that the selector applies
                            to.
                          type: string
                        operator:
                          description: |-
                            operator represents a key's relationship to a set of values.
                            Valid operators are In, NotIn, Exists and DoesNotExist.
                          type: string
                        values:
                          description: |-
                            values is an array of string values. If the operator is In or NotIn,
                            the values array must be non-empty. If the operator is Exists or DoesNotExist,
                            the values array must be empty. This array is replaced during a strategic
                            merge patch.
                          items:
                            type: string
                          type: array
                      required:
                      - key
                      - operator
                      type: object
                    type: array
                  matchLabels:
                    additionalProperties:
                      type: string
                    description: |-
                      matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels
                      map is equivalent to an element of matchExpressions, whose key field is "key", the
                      operator is "In", and the values array contains only "value". The requirements are ANDed.
                    type: object
                type: object
                x-kubernetes-map-type: atomic
              serviceName:
                description: |-
                  serviceName is the name of the service that governs this StatefulSet.
                  This service must exist before the StatefulSet, and is responsible for
                  the network identity of the set. Pods get DNS/hostnames that follow the
                  pattern: pod-specific-string.serviceName.default.svc.cluster.local
                  where "pod-specific-string" is managed by the StatefulSet controller.
                type: string
              template:
                description: |-
                  template is the object that describes the pod that will be created if
                  insufficient replicas are detected. Each pod stamped out by the StatefulSet
                  will fulfill this Template, but have a unique identity from the rest
                  of the StatefulSet.
                x-kubernetes-preserve-unknown-fields: true
              updateStrategy:
                description: |-
                  updateStrategy indicates the StatefulSetUpdateStrategy that will be
                  employed to update Pods in the StatefulSet when a revision is made to
                  Template.
                properties:
                  rollingUpdate:
                    description: RollingUpdate is used to communicate parameters when
                      Type is RollingUpdateStatefulSetStrategyType.
                    properties:
                      inPlaceUpdateStrategy:
                        description: InPlaceUpdateStrategy contains strategies for
                          in-place update.
                        properties:
                          gracePeriodSeconds:
                            description: |-
                              GracePeriodSeconds is the timespan between set Pod status to not-ready and update images in Pod spec
                              when in-place update a Pod.
                            format: int32
                            type: integer
                        type: object
                      maxUnavailable:
                        anyOf:
                        - type: integer
                        - type: string
                        description: |-
                          The maximum number of pods that can be unavailable during the update.
                          Value can be an absolute number (ex: 5) or a percentage of desired pods (ex: 10%).
                          Absolute number is calculated from percentage by rounding down.
                          Also, maxUnavailable can just be allowed to work with Parallel podManagementPolicy.
                          Defaults to 1.
                        x-kubernetes-int-or-string: true
                      minReadySeconds:
                        description: |-
                          MinReadySeconds indicates how long will the pod be considered ready after it's updated.
                          MinReadySeconds works with both OrderedReady and Parallel podManagementPolicy.
                          It affects the pod scale up speed when the podManagementPolicy is set to be OrderedReady.
                          Combined with MaxUnavailable, it affects the pod update speed regardless of podManagementPolicy.
                          Default value is 0, max is 300.
                        format: int32
                        type: integer
                      partition:
                        description: |-
                          Partition indicates the ordinal at which the StatefulSet should be partitioned by default.
                          But if unorderedUpdate has been set:
                            - Partition indicates the number of pods with non-updated revisions when rolling update.
                            - It means controller will update $(replicas - partition) number of pod.
                          Default value is 0.
                        format: int32
                        type: integer
                      paused:
                        description: |-
                          Paused indicates that the StatefulSet is paused.
                          Default value is false
                        type: boolean
                      podUpdatePolicy:
                        description: |-
                          PodUpdatePolicy indicates how pods should be updated
                          Default value is "ReCreate"
                        type: string
                      unorderedUpdate:
                        description: |-
                          UnorderedUpdate contains strategies for non-ordered update.
                          If it is not nil, pods will be updated with non-ordered sequence.
                          Noted that UnorderedUpdate can only be allowed to work with Parallel podManagementPolicy
                        properties:
                          priorityStrategy:
                            description: |-
                              Priorities are the rules for calculating the priority of updating pods.
                              Each pod to be updated, will pass through these terms and get a sum of weights.
                            properties:
                              orderPriority:
                                description: |-
                                  Order priority terms, pods will be sorted by the value of orderedKey.
                                  For example:
                                  '''
                                  orderPriority:
			                      - orderedKey: key1
                                  - orderedKey: key2
                                  '''
                                  First, all pods which have key1 in labels will be sorted by the value of key1.
                                  Then, the left pods which have no key1 but have key2 in labels will be sorted by
                                  the value of key2 and put behind those pods have key1.
                                items:
                                  description: UpdatePriorityOrderTerm defines order
                                    priority.
                                  properties:
                                    orderedKey:
                                      description: |-
                                        Calculate priority by value of this key.
                                        Values of this key, will be sorted by GetInt(val). GetInt method will find the last int in value,
                                        such as getting 5 in value '5', getting 10 in value 'sts-10'.
                                      type: string
                                  required:
                                  - orderedKey
                                  type: object
                                type: array
                              weightPriority:
                                description: Weight priority terms, pods will be sorted
                                  by the sum of all terms weight.
                                items:
                                  description: UpdatePriorityWeightTerm defines weight
                                    priority.
                                  properties:
                                    matchSelector:
                                      description: MatchSelector is used to select
                                        by pod's labels.
                                      properties:
                                        matchExpressions:
                                          description: matchExpressions is a list
                                            of label selector requirements. The requirements
                                            are ANDed.
                                          items:
                                            description: |-
                                              A label selector requirement is a selector that contains values, a key, and an operator that
                                              relates the key and values.
                                            properties:
                                              key:
                                                description: key is the label key
                                                  that the selector applies to.
                                                type: string
                                              operator:
                                                description: |-
                                                  operator represents a key's relationship to a set of values.
                                                  Valid operators are In, NotIn, Exists and DoesNotExist.
                                                type: string
                                              values:
                                                description: |-
                                                  values is an array of string values. If the operator is In or NotIn,
                                                  the values array must be non-empty. If the operator is Exists or DoesNotExist,
                                                  the values array must be empty. This array is replaced during a strategic
                                                  merge patch.
                                                items:
                                                  type: string
                                                type: array
                                            required:
                                            - key
                                            - operator
                                            type: object
                                          type: array
                                        matchLabels:
                                          additionalProperties:
                                            type: string
                                          description: |-
                                            matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels
                                            map is equivalent to an element of matchExpressions, whose key field is "key", the
                                            operator is "In", and the values array contains only "value". The requirements are ANDed.
                                          type: object
                                      type: object
                                      x-kubernetes-map-type: atomic
                                    weight:
                                      description: Weight associated with matching
                                        the corresponding matchExpressions, in the
                                        range 1-100.
                                      format: int32
                                      type: integer
                                  required:
                                  - matchSelector
                                  - weight
                                  type: object
                                type: array
                            type: object
                        type: object
                    type: object
                  type:
                    description: |-
                      Type indicates the type of the StatefulSetUpdateStrategy.
                      Default is RollingUpdate.
                    type: string
                type: object
              volumeClaimTemplates:
                description: |-
                  volumeClaimTemplates is a list of claims that pods are allowed to reference.
                  The StatefulSet controller is responsible for mapping network identities to
                  claims in a way that maintains the identity of a pod. Every claim in
                  this list must have at least one matching (by name) volumeMount in one
                  container in the template. A claim in this list takes precedence over
                  any volumes in the template, with the same name.
                  TODO: Define the behavior if a claim already exists with the same name.
                x-kubernetes-preserve-unknown-fields: true
            required:
            - selector
            - template
            type: object
          status:
            description: StatefulSetStatus defines the observed state of StatefulSet
            properties:
              availableReplicas:
                description: |-
                  AvailableReplicas is the number of Pods created by the StatefulSet controller that have been ready for
                  minReadySeconds.
                format: int32
                type: integer
              collisionCount:
                description: |-
                  collisionCount is the count of hash collisions for the StatefulSet. The StatefulSet controller
                  uses this field as a collision avoidance mechanism when it needs to create the name for the
                  newest ControllerRevision.
                format: int32
                type: integer
              conditions:
                description: Represents the latest available observations of a statefulset's
                  current state.
                items:
                  description: StatefulSetCondition describes the state of a statefulset
                    at a certain point.
                  properties:
                    lastTransitionTime:
                      description: Last time the condition transitioned from one status
                        to another.
                      format: date-time
                      type: string
                    message:
                      description: A human readable message indicating details about
                        the transition.
                      type: string
                    reason:
                      description: The reason for the condition's last transition.
                      type: string
                    status:
                      description: Status of the condition, one of True, False, Unknown.
                      type: string
                    type:
                      description: Type of statefulset condition.
                      type: string
                  required:
                  - status
                  - type
                  type: object
                type: array
              currentReplicas:
                description: |-
                  currentReplicas is the number of Pods created by the StatefulSet controller from the StatefulSet version
                  indicated by currentRevision.
                format: int32
                type: integer
              currentRevision:
                description: |-
                  currentRevision, if not empty, indicates the version of the StatefulSet used to generate Pods in the
                  sequence [0,currentReplicas).
                type: string
              labelSelector:
                description: LabelSelector is label selectors for query over pods
                  that should match the replica count used by HPA.
                type: string
              observedGeneration:
                description: |-
                  observedGeneration is the most recent generation observed for this StatefulSet. It corresponds to the
                  StatefulSet's generation, which is updated on mutation by the API Server.
                format: int64
                type: integer
              readyReplicas:
                description: readyReplicas is the number of Pods created by the StatefulSet
                  controller that have a Ready Condition.
                format: int32
                type: integer
              replicas:
                description: replicas is the number of Pods created by the StatefulSet
                  controller.
                format: int32
                type: integer
              updateRevision:
                description: |-
                  updateRevision, if not empty, indicates the version of the StatefulSet used to generate Pods in the sequence
                  [replicas-updatedReplicas,replicas)
                type: string
              updatedReplicas:
                description: |-
                  updatedReplicas is the number of Pods created by the StatefulSet controller from the StatefulSet version
                  indicated by updateRevision.
                format: int32
                type: integer
            required:
            - availableReplicas
            - currentReplicas
            - readyReplicas
            - replicas
            - updatedReplicas
            type: object
        type: object
    served: true
    storage: false
    subresources:
      scale:
        labelSelectorPath: .status.labelSelector
        specReplicasPath: .spec.replicas
        statusReplicasPath: .status.replicas
      status: {}
  - additionalPrinterColumns:
    - description: The desired number of pods.
      jsonPath: .spec.replicas
      name: DESIRED
      type: integer
    - description: The number of currently all pods.
      jsonPath: .status.replicas
      name: CURRENT
      type: integer
    - description: The number of pods updated.
      jsonPath: .status.updatedReplicas
      name: UPDATED
      type: integer
    - description: The number of pods ready.
      jsonPath: .status.readyReplicas
      name: READY
      type: integer
    - description: CreationTimestamp is a timestamp representing the server time when
        this object was created. It is not guaranteed to be set in happens-before
        order across separate operations. Clients may not set this value. It is represented
        in RFC3339 form and is in UTC.
      jsonPath: .metadata.creationTimestamp
      name: AGE
      type: date
    - description: The containers of currently advanced statefulset.
      jsonPath: .spec.template.spec.containers[*].name
      name: CONTAINERS
      priority: 1
      type: string
    - description: The images of currently advanced statefulset.
      jsonPath: .spec.template.spec.containers[*].image
      name: IMAGES
      priority: 1
      type: string
    name: v1beta1
    schema:
      openAPIV3Schema:
        description: StatefulSet is the Schema for the statefulsets API
        properties:
          apiVersion:
            description: |-
              APIVersion defines the versioned schema of this representation of an object.
              Servers should convert recognized schemas to the latest internal value, and
              may reject unrecognized values.
              More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
            type: string
          kind:
            description: |-
              Kind is a string value representing the REST resource this object represents.
              Servers may infer this from the endpoint the client submits requests to.
              Cannot be updated.
              In CamelCase.
              More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
            type: string
          metadata:
            type: object
          spec:
            description: StatefulSetSpec defines the desired state of StatefulSet
            properties:
              lifecycle:
                description: Lifecycle defines the lifecycle hooks for Pods pre-delete,
                  in-place update.
                properties:
                  inPlaceUpdate:
                    description: InPlaceUpdate is the hook before Pod to update and
                      after Pod has been updated.
                    properties:
                      finalizersHandler:
                        items:
                          type: string
                        type: array
                      labelsHandler:
                        additionalProperties:
                          type: string
                        type: object
                      markPodNotReady:
                        description: |-
                          MarkPodNotReady = true means:
                          - Pod will be set to 'NotReady' at preparingDelete/preparingUpdate state.
                          - Pod will be restored to 'Ready' at Updated state if it was set to 'NotReady' at preparingUpdate state.
                          Currently, MarkPodNotReady only takes effect on InPlaceUpdate & PreDelete hook.
                          Default to false.
                        type: boolean
                    type: object
                  preDelete:
                    description: PreDelete is the hook before Pod to be deleted.
                    properties:
                      finalizersHandler:
                        items:
                          type: string
                        type: array
                      labelsHandler:
                        additionalProperties:
                          type: string
                        type: object
                      markPodNotReady:
                        description: |-
                          MarkPodNotReady = true means:
                          - Pod will be set to 'NotReady' at preparingDelete/preparingUpdate state.
                          - Pod will be restored to 'Ready' at Updated state if it was set to 'NotReady' at preparingUpdate state.
                          Currently, MarkPodNotReady only takes effect on InPlaceUpdate & PreDelete hook.
                          Default to false.
                        type: boolean
                    type: object
                  preNormal:
                    description: PreNormal is the hook after Pod to be created and
                      ready to be Normal.
                    properties:
                      finalizersHandler:
                        items:
                          type: string
                        type: array
                      labelsHandler:
                        additionalProperties:
                          type: string
                        type: object
                      markPodNotReady:
                        description: |-
                          MarkPodNotReady = true means:
                          - Pod will be set to 'NotReady' at preparingDelete/preparingUpdate state.
                          - Pod will be restored to 'Ready' at Updated state if it was set to 'NotReady' at preparingUpdate state.
                          Currently, MarkPodNotReady only takes effect on InPlaceUpdate & PreDelete hook.
                          Default to false.
                        type: boolean
                    type: object
                type: object
              ordinals:
                description: |-
                  ordinals controls the numbering of replica indices in a StatefulSet. The
                  default ordinals behavior assigns a "0" index to the first replica and
                  increments the index by one for each additional replica requested. Using
                  the ordinals field requires the StatefulSetStartOrdinal feature gate to be
                  enabled, which is beta.
                properties:
                  start:
                    description: |-
                      start is the number representing the first replica's index. It may be used
                      to number replicas from an alternate index (eg: 1-indexed) over the default
                      0-indexed names, or to orchestrate progressive movement of replicas from
                      one StatefulSet to another.
                      If set, replica indices will be in the range:
                        [.spec.ordinals.start, .spec.ordinals.start + .spec.replicas).
                      If unset, defaults to 0. Replica indices will be in the range:
                        [0, .spec.replicas).
                    format: int32
                    type: integer
                type: object
              persistentVolumeClaimRetentionPolicy:
                description: |-
                  PersistentVolumeClaimRetentionPolicy describes the policy used for PVCs created from
                  the StatefulSet VolumeClaimTemplates. This requires the
                  StatefulSetAutoDeletePVC feature gate to be enabled, which is alpha.
                properties:
                  whenDeleted:
                    description: |-
                      WhenDeleted specifies what happens to PVCs created from StatefulSet
                      VolumeClaimTemplates when the StatefulSet is deleted. The default policy
                      of 'Retain' causes PVCs to not be affected by StatefulSet deletion. The
                      'Delete' policy causes those PVCs to be deleted.
                    type: string
                  whenScaled:
                    description: |-
                      WhenScaled specifies what happens to PVCs created from StatefulSet
                      VolumeClaimTemplates when the StatefulSet is scaled down. The default
                      policy of 'Retain' causes PVCs to not be affected by a scaledown. The
                      'Delete' policy causes the associated PVCs for any excess pods above
                      the replica count to be deleted.
                    type: string
                type: object
              podManagementPolicy:
                description: |-
                  podManagementPolicy controls how pods are created during initial scale up,
                  when replacing pods on nodes, or when scaling down. The default policy is
                  'OrderedReady', where pods are created in increasing order (pod-0, then
                  pod-1, etc) and the controller will wait until each pod is ready before
                  continuing. When scaling down, the pods are removed in the opposite order.
                  The alternative policy is 'Parallel' which will create pods in parallel
                  to match the desired scale without waiting, and on scale down will delete
                  all pods at once.
                type: string
              replicas:
                description: |-
                  replicas is the desired number of replicas of the given Template.
                  These are replicas in the sense that they are instantiations of the
                  same Template, but individual replicas also have a consistent identity.
                  If unspecified, defaults to 1.
                  TODO: Consider a rename of this field.
                format: int32
                type: integer
              reserveOrdinals:
                description: |-
                  reserveOrdinals controls the ordinal numbers that should be reserved, and the replicas
                  will always be the expectation number of running Pods.
                  For a sts with replicas=3 and its Pods in [0, 1, 2]:
                  - If you want to migrate Pod-1 and reserve this ordinal, just set spec.reserveOrdinal to [1].
                    Then controller will delete Pod-1 and create Pod-3 (existing Pods will be [0, 2, 3])
                  - If you just want to delete Pod-1, you should set spec.reserveOrdinal to [1] and spec.replicas to 2.
                    Then controller will delete Pod-1 (existing Pods will be [0, 2])
                items:
                  type: integer
                type: array
              revisionHistoryLimit:
                description: |-
                  revisionHistoryLimit is the maximum number of revisions that will
                  be maintained in the StatefulSet's revision history. The revision history
                  consists of all revisions not represented by a currently applied
                  StatefulSetSpec version. The default value is 10.
                format: int32
                type: integer
              scaleStrategy:
                description: |-
                  scaleStrategy indicates the StatefulSetScaleStrategy that will be
                  employed to scale Pods in the StatefulSet.
                properties:
                  maxUnavailable:
                    anyOf:
                    - type: integer
                    - type: string
                    description: |-
                      The maximum number of pods that can be unavailable during scaling.
                      Value can be an absolute number (ex: 5) or a percentage of desired pods (ex: 10%).
                      Absolute number is calculated from percentage by rounding down.
                      It can just be allowed to work with Parallel podManagementPolicy.
                    x-kubernetes-int-or-string: true
                type: object
              selector:
                description: |-
                  selector is a label query over pods that should match the replica count.
                  It must match the pod template's labels.
                  More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors
                properties:
                  matchExpressions:
                    description: matchExpressions is a list of label selector requirements.
                      The requirements are ANDed.
                    items:
                      description: |-
                        A label selector requirement is a selector that contains values, a key, and an operator that
                        relates the key and values.
                      properties:
                        key:
                          description: key is the label key that the selector applies
                            to.
                          type: string
                        operator:
                          description: |-
                            operator represents a key's relationship to a set of values.
                            Valid operators are In, NotIn, Exists and DoesNotExist.
                          type: string
                        values:
                          description: |-
                            values is an array of string values. If the operator is In or NotIn,
                            the values array must be non-empty. If the operator is Exists or DoesNotExist,
                            the values array must be empty. This array is replaced during a strategic
                            merge patch.
                          items:
                            type: string
                          type: array
                      required:
                      - key
                      - operator
                      type: object
                    type: array
                  matchLabels:
                    additionalProperties:
                      type: string
                    description: |-
                      matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels
                      map is equivalent to an element of matchExpressions, whose key field is "key", the
                      operator is "In", and the values array contains only "value". The requirements are ANDed.
                    type: object
                type: object
                x-kubernetes-map-type: atomic
              serviceName:
                description: |-
                  serviceName is the name of the service that governs this StatefulSet.
                  This service must exist before the StatefulSet, and is responsible for
                  the network identity of the set. Pods get DNS/hostnames that follow the
                  pattern: pod-specific-string.serviceName.default.svc.cluster.local
                  where "pod-specific-string" is managed by the StatefulSet controller.
                type: string
              template:
                description: |-
                  template is the object that describes the pod that will be created if
                  insufficient replicas are detected. Each pod stamped out by the StatefulSet
                  will fulfill this Template, but have a unique identity from the rest
                  of the StatefulSet.
                x-kubernetes-preserve-unknown-fields: true
              updateStrategy:
                description: |-
                  updateStrategy indicates the StatefulSetUpdateStrategy that will be
                  employed to update Pods in the StatefulSet when a revision is made to
                  Template.
                properties:
                  rollingUpdate:
                    description: RollingUpdate is used to communicate parameters when
                      Type is RollingUpdateStatefulSetStrategyType.
                    properties:
                      inPlaceUpdateStrategy:
                        description: InPlaceUpdateStrategy contains strategies for
                          in-place update.
                        properties:
                          gracePeriodSeconds:
                            description: |-
                              GracePeriodSeconds is the timespan between set Pod status to not-ready and update images in Pod spec
                              when in-place update a Pod.
                            format: int32
                            type: integer
                        type: object
                      maxUnavailable:
                        anyOf:
                        - type: integer
                        - type: string
                        description: |-
                          The maximum number of pods that can be unavailable during the update.
                          Value can be an absolute number (ex: 5) or a percentage of desired pods (ex: 10%).
                          Absolute number is calculated from percentage by rounding down.
                          Also, maxUnavailable can just be allowed to work with Parallel podManagementPolicy.
                          Defaults to 1.
                        x-kubernetes-int-or-string: true
                      minReadySeconds:
                        description: |-
                          MinReadySeconds indicates how long will the pod be considered ready after it's updated.
                          MinReadySeconds works with both OrderedReady and Parallel podManagementPolicy.
                          It affects the pod scale up speed when the podManagementPolicy is set to be OrderedReady.
                          Combined with MaxUnavailable, it affects the pod update speed regardless of podManagementPolicy.
                          Default value is 0, max is 300.
                        format: int32
                        type: integer
                      partition:
                        description: |-
                          Partition indicates the ordinal at which the StatefulSet should be partitioned by default.
                          But if unorderedUpdate has been set:
                            - Partition indicates the number of pods with non-updated revisions when rolling update.
                            - It means controller will update $(replicas - partition) number of pod.
                          Default value is 0.
                        format: int32
                        type: integer
                      paused:
                        description: |-
                          Paused indicates that the StatefulSet is paused.
                          Default value is false
                        type: boolean
                      podUpdatePolicy:
                        description: |-
                          PodUpdatePolicy indicates how pods should be updated
                          Default value is "ReCreate"
                        type: string
                      unorderedUpdate:
                        description: |-
                          UnorderedUpdate contains strategies for non-ordered update.
                          If it is not nil, pods will be updated with non-ordered sequence.
                          Noted that UnorderedUpdate can only be allowed to work with Parallel podManagementPolicy
                        properties:
                          priorityStrategy:
                            description: |-
                              Priorities are the rules for calculating the priority of updating pods.
                              Each pod to be updated, will pass through these terms and get a sum of weights.
                            properties:
                              orderPriority:
                                description: |-
                                  Order priority terms, pods will be sorted by the value of orderedKey.
                                  For example:
                                  '''
                                  orderPriority:
                                  - orderedKey: key1
                                  - orderedKey: key2
                                  '''
                                  First, all pods which have key1 in labels will be sorted by the value of key1.
                                  Then, the left pods which have no key1 but have key2 in labels will be sorted by
                                  the value of key2 and put behind those pods have key1.
                                items:
                                  description: UpdatePriorityOrderTerm defines order
                                    priority.
                                  properties:
                                    orderedKey:
                                      description: |-
                                        Calculate priority by value of this key.
                                        Values of this key, will be sorted by GetInt(val). GetInt method will find the last int in value,
                                        such as getting 5 in value '5', getting 10 in value 'sts-10'.
                                      type: string
                                  required:
                                  - orderedKey
                                  type: object
                                type: array
                              weightPriority:
                                description: Weight priority terms, pods will be sorted
                                  by the sum of all terms weight.
                                items:
                                  description: UpdatePriorityWeightTerm defines weight
                                    priority.
                                  properties:
                                    matchSelector:
                                      description: MatchSelector is used to select
                                        by pod's labels.
                                      properties:
                                        matchExpressions:
                                          description: matchExpressions is a list
                                            of label selector requirements. The requirements
                                            are ANDed.
                                          items:
                                            description: |-
                                              A label selector requirement is a selector that contains values, a key, and an operator that
                                              relates the key and values.
                                            properties:
                                              key:
                                                description: key is the label key
                                                  that the selector applies to.
                                                type: string
                                              operator:
                                                description: |-
                                                  operator represents a key's relationship to a set of values.
                                                  Valid operators are In, NotIn, Exists and DoesNotExist.
                                                type: string
                                              values:
                                                description: |-
                                                  values is an array of string values. If the operator is In or NotIn,
                                                  the values array must be non-empty. If the operator is Exists or DoesNotExist,
                                                  the values array must be empty. This array is replaced during a strategic
                                                  merge patch.
                                                items:
                                                  type: string
                                                type: array
                                            required:
                                            - key
                                            - operator
                                            type: object
                                          type: array
                                        matchLabels:
                                          additionalProperties:
                                            type: string
                                          description: |-
                                            matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels
                                            map is equivalent to an element of matchExpressions, whose key field is "key", the
                                            operator is "In", and the values array contains only "value". The requirements are ANDed.
                                          type: object
                                      type: object
                                      x-kubernetes-map-type: atomic
                                    weight:
                                      description: Weight associated with matching
                                        the corresponding matchExpressions, in the
                                        range 1-100.
                                      format: int32
                                      type: integer
                                  required:
                                  - matchSelector
                                  - weight
                                  type: object
                                type: array
                            type: object
                        type: object
                    type: object
                  type:
                    description: |-
                      Type indicates the type of the StatefulSetUpdateStrategy.
                      Default is RollingUpdate.
                    type: string
                type: object
              volumeClaimTemplates:
                description: |-
                  volumeClaimTemplates is a list of claims that pods are allowed to reference.
                  The StatefulSet controller is responsible for mapping network identities to
                  claims in a way that maintains the identity of a pod. Every claim in
                  this list must have at least one matching (by name) volumeMount in one
                  container in the template. A claim in this list takes precedence over
                  any volumes in the template, with the same name.
                  TODO: Define the behavior if a claim already exists with the same name.
                x-kubernetes-preserve-unknown-fields: true
            required:
            - selector
            - template
            type: object
          status:
            description: StatefulSetStatus defines the observed state of StatefulSet
            properties:
              availableReplicas:
                description: |-
                  AvailableReplicas is the number of Pods created by the StatefulSet controller that have been ready for
                  minReadySeconds.
                format: int32
                type: integer
              collisionCount:
                description: |-
                  collisionCount is the count of hash collisions for the StatefulSet. The StatefulSet controller
                  uses this field as a collision avoidance mechanism when it needs to create the name for the
                  newest ControllerRevision.
                format: int32
                type: integer
              conditions:
                description: Represents the latest available observations of a statefulset's
                  current state.
                items:
                  description: StatefulSetCondition describes the state of a statefulset
                    at a certain point.
                  properties:
                    lastTransitionTime:
                      description: Last time the condition transitioned from one status
                        to another.
                      format: date-time
                      type: string
                    message:
                      description: A human readable message indicating details about
                        the transition.
                      type: string
                    reason:
                      description: The reason for the condition's last transition.
                      type: string
                    status:
                      description: Status of the condition, one of True, False, Unknown.
                      type: string
                    type:
                      description: Type of statefulset condition.
                      type: string
                  required:
                  - status
                  - type
                  type: object
                type: array
              currentReplicas:
                description: |-
                  currentReplicas is the number of Pods created by the StatefulSet controller from the StatefulSet version
                  indicated by currentRevision.
                format: int32
                type: integer
              currentRevision:
                description: |-
                  currentRevision, if not empty, indicates the version of the StatefulSet used to generate Pods in the
                  sequence [0,currentReplicas).
                type: string
              labelSelector:
                description: LabelSelector is label selectors for query over pods
                  that should match the replica count used by HPA.
                type: string
              observedGeneration:
                description: |-
                  observedGeneration is the most recent generation observed for this StatefulSet. It corresponds to the
                  StatefulSet's generation, which is updated on mutation by the API Server.
                format: int64
                type: integer
              readyReplicas:
                description: readyReplicas is the number of Pods created by the StatefulSet
                  controller that have a Ready Condition.
                format: int32
                type: integer
              replicas:
                description: replicas is the number of Pods created by the StatefulSet
                  controller.
                format: int32
                type: integer
              updateRevision:
                description: |-
                  updateRevision, if not empty, indicates the version of the StatefulSet used to generate Pods in the sequence
                  [replicas-updatedReplicas,replicas)
                type: string
              updatedAvailableReplicas:
                description: |-
                  updatedAvailableReplicas is the number of updated Pods created by the StatefulSet controller that have a Ready condition
                  for atleast minReadySeconds.
                format: int32
                type: integer
              updatedReadyReplicas:
                description: updatedReadyReplicas is the number of updated Pods created
                  by the StatefulSet controller that have a Ready Condition.
                format: int32
                type: integer
              updatedReplicas:
                description: |-
                  updatedReplicas is the number of Pods created by the StatefulSet controller from the StatefulSet version
                  indicated by updateRevision.
                format: int32
                type: integer
            required:
            - availableReplicas
            - currentReplicas
            - readyReplicas
            - replicas
            - updatedReplicas
            type: object
        type: object
    served: true
    storage: true
    subresources:
      scale:
        labelSelectorPath: .status.labelSelector
        specReplicasPath: .spec.replicas
        statusReplicasPath: .status.replicas
      status: {}
`)

var KruiseCRDs = []*apiextensionsv1.CustomResourceDefinition{
	object.Unmarshal[apiextensionsv1.CustomResourceDefinition](KruiseDaemonSetCRD),
}
