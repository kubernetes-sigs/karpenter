package termination

import (
	"errors"
)

type NodeDrainError struct {
	Err error
}

func (e *NodeDrainError) Error() string {
	return e.Err.Error()
}

func NewNodeDrainError(err error) *NodeDrainError {
	return &NodeDrainError{Err: err}
}

func IsNodeDrainError(err error) bool {
	if err == nil {
		return false
	}
	var nodeDrainErr *NodeDrainError
	return errors.As(err, &nodeDrainErr)
}
