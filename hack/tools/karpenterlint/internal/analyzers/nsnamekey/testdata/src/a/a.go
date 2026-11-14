package a

import (
	"unique"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
)

// Shape of kubernetes-sigs/karpenter#3102.
func claimID(claim *resourcev1.ResourceClaim) unique.Handle[string] {
	return unique.Make(claim.Name) // want `unique handle uses only the name of namespaced ResourceClaim`
}

func index(pods []*corev1.Pod) map[string]*corev1.Pod {
	out := map[string]*corev1.Pod{}
	for _, p := range pods {
		out[p.Name] = p // want `map key uses only the name of namespaced Pod`
		out[p.GetName()] = p // want `map key uses only the name of namespaced Pod`
		out[p.ObjectMeta.Name] = p // want `map key uses only the name of namespaced Pod`
	}
	return out
}

// Nodes are cluster-scoped.
func nodes(ns []*corev1.Node) map[string]*corev1.Node {
	out := map[string]*corev1.Node{}
	for _, n := range ns {
		out[n.Name] = n
	}
	return out
}

func namespacedKey(p *corev1.Pod) map[string]bool {
	return map[string]bool{p.Namespace + "/" + p.Name: true}
}
