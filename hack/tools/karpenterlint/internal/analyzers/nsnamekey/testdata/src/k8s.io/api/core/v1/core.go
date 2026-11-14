package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type Pod struct{ metav1.ObjectMeta }

type Node struct{ metav1.ObjectMeta }
