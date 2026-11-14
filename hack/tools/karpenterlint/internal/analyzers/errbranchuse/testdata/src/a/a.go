package a

import "errors"

type Command struct{ Candidates []string }

func (c Command) EmitRejectedEvents() {}

func validate(c Command) (Command, error) { return Command{}, errors.New("invalid") }

// Shape of kubernetes-sigs/karpenter#3350.
func bug(cmd Command) {
	var err error
	if cmd, err = validate(cmd); err != nil {
		cmd.EmitRejectedEvents() // want `cmd was overwritten by the failing call`
	}
}

func bugSeparateStmt(cmd Command) {
	var err error
	cmd, err = validate(cmd)
	if err != nil {
		_ = cmd.Candidates // want `cmd was overwritten by the failing call`
	}
}

func fixed(cmd Command) {
	validated, err := validate(cmd)
	if err != nil {
		cmd.EmitRejectedEvents()
		return
	}
	_ = validated
}

func unrelatedInput(cmd, other Command) {
	var err error
	if cmd, err = validate(other); err != nil {
		_ = cmd
	}
}
