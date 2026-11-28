package b

import (
	"a"

	"sigs.k8s.io/karpenter/pkg/labels"
)

var derived = a.Marked + "-suffix" // want `Marked is bound by labels.Register; reading it in a package-level initializer`

var keys []string

func init() {
	keys = append(keys, a.Single, a.Unmarked) // want `Single is bound by labels.Register; reading it in init\(\)`
}

func Read() string { return a.Marked }

var Options = []labels.Option{
	{Key: "b.example.com/key", Binds: []*string{&a.Marked}},
	{Key: "b.example.com/other", Binds: []*string{&a.Unmarked}}, // want `&a.Unmarked is in Binds but a.Unmarked isn't marked`
}

var _ = derived
