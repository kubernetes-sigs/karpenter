package a

import (
	"strings"

	"sigs.k8s.io/karpenter/pkg/labels"
)

var (
	// Marked is bound by labels.Register.
	// +labels:bound
	Marked = "a.example.com/marked" // want Marked:"labels:bound"
	// +labels:bound
	Other = "a.example.com/other" // want Other:"labels:bound"

	Unmarked = "a.example.com/unmarked"
)

// +labels:bound
var Single = "a.example.com/single" // want Single:"labels:bound"

// A marker on a parenthesized group doesn't mark its vars.
// +labels:bound
var (
	Grouped = "a.example.com/grouped"
)

var (
	derived       = Marked + "-suffix"               // want `Marked is bound by labels.Register; reading it in a package-level initializer sees the key before Register runs`
	derivedSingle = strings.ToUpper(Single)          // want `Single is bound by labels.Register; reading it in a package-level initializer`
	invoked       = func() string { return Other }() // want `Other is bound by labels.Register; reading it in a package-level initializer`
	lazy          = func() string { return Marked }
	address       = &Marked
	unmarkedRead  = Unmarked + Grouped
)

var reserved = map[string]bool{}

func init() {
	reserved[Marked] = true // want `Marked is bound by labels.Register; reading it in init\(\) sees the key before Register runs`
	Other = "a.example.com/overwritten"
	Other += "-suffix"         // want `Other is bound by labels.Register; reading it in init\(\)`
	go func() { _ = Marked }() // want `Marked is bound by labels.Register; reading it in init\(\)`
	register(func() string { return Marked })
	reserved[Unmarked] = true
}

func register(func() string) {}

// Reading a marked var in a function that runs after Register is fine.
func Read() string { return Marked + Single }

func Options() []labels.Option {
	return []labels.Option{
		{Key: Marked, Binds: []*string{&Marked, &Other}},
		{Key: Single, Binds: []*string{&Single}},
		{Key: Unmarked, Binds: []*string{&Unmarked}}, // want `&Unmarked is in Binds but Unmarked isn't marked // \+labels:bound`
		{Key: Grouped, Binds: []*string{&Grouped}},   // want `&Grouped is in Binds but Grouped isn't marked`
	}
}

func Pointers() []*labels.Option {
	return []*labels.Option{
		{Key: Marked, Binds: []*string{&Marked}},
		{Key: Unmarked, Binds: []*string{&Unmarked}}, // want `&Unmarked is in Binds but Unmarked isn't marked`
	}
}

func Pointer() *labels.Option {
	local := "a.example.com/local"
	return &labels.Option{Key: local, Binds: []*string{&local}} // want `&local is in Binds but local isn't marked`
}

func Assign() labels.Option {
	o := labels.Option{Key: Marked}
	o.Binds = []*string{&Marked, &Unmarked}      // want `&Unmarked is in Binds but Unmarked isn't marked`
	o.Binds = append(o.Binds, &Other, &Unmarked) // want `&Unmarked is in Binds but Unmarked isn't marked`
	p := &o
	p.Binds = append(p.Binds, &Single, &Grouped) // want `&Grouped is in Binds but Grouped isn't marked`
	unchecked := []*string{&Unmarked}
	o.Binds = append(o.Binds, unchecked...)
	return o
}

func NonAddress(ptr *string) labels.Option {
	return labels.Option{Key: Marked, Binds: []*string{ptr}} // want `ptr is in Binds but isn't &X of a var marked // \+labels:bound`
}

var _, _, _, _, _, _ = derived, derivedSingle, invoked, lazy, address, unmarkedRead
