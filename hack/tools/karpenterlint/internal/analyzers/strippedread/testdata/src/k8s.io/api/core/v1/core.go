package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ContainerImage struct{ Names []string }

type NodeStatus struct {
	Phase  string
	Images []ContainerImage
}

type Node struct {
	metav1.ObjectMeta
	Status NodeStatus
}

// ImageList has an Images field that isn't stripped.
type ImageList struct {
	Images []ContainerImage
}
