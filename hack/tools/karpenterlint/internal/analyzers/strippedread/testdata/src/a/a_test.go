package a

import corev1 "k8s.io/api/core/v1"

func testReads(node *corev1.Node) {
	_ = node.ManagedFields
	_ = node.Status.Images
}
