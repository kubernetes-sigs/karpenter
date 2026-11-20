package a

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type NodeAlias = corev1.Node

func reads(node *corev1.Node, alias *NodeAlias, obj metav1.Object, meta metav1.ObjectMeta, status corev1.NodeStatus) {
	_ = node.ManagedFields            // want `ObjectMeta.ManagedFields is stripped from cached objects`
	_ = node.ObjectMeta.ManagedFields // want `ObjectMeta.ManagedFields is stripped from cached objects`
	_ = node.GetManagedFields()       // want `ObjectMeta.ManagedFields is stripped from cached objects`
	_ = obj.GetManagedFields()        // want `ObjectMeta.ManagedFields is stripped from cached objects`
	_ = meta.ManagedFields            // want `ObjectMeta.ManagedFields is stripped from cached objects`
	_ = node.Status.Images            // want `NodeStatus.Images is stripped from cached objects`
	_ = alias.Status.Images           // want `NodeStatus.Images is stripped from cached objects`
	_ = len(status.Images)            // want `NodeStatus.Images is stripped from cached objects`
	get := node.GetManagedFields      // want `ObjectMeta.ManagedFields is stripped from cached objects`
	_ = get
}

func notReads(node *corev1.Node, list corev1.ImageList, obj metav1.Object) {
	node.ManagedFields = nil
	(node.Status.Images) = nil
	node.SetManagedFields(nil)
	_ = node.Name
	_ = node.GetName()
	_ = obj.GetName()
	_ = node.Status.Phase
	_ = list.Images
	_ = corev1.NodeStatus{Images: nil}
}
