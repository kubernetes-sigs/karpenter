package status

type Condition struct{ Type string }

type ForOptions struct{ ObservedOnly bool }

type ForOption func(*ForOptions)

func WithObservedOnly() ForOption { return func(o *ForOptions) { o.ObservedOnly = true } }

type ConditionSet struct{}

func (c ConditionSet) Root() *Condition                     { return nil }
func (c ConditionSet) List() []Condition                    { return nil }
func (c ConditionSet) Get(t string) *Condition              { return nil }
func (c ConditionSet) IsTrue(t ...string) bool              { return false }
func (c ConditionSet) IsDependentCondition(t string) bool   { return false }
func (c ConditionSet) SetTrue(t string) bool                { return false }
func (c ConditionSet) SetFalse(t, reason, message string) bool { return false }
func (c ConditionSet) Clear(t string) error                 { return nil }
