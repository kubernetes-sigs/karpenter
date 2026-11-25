/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scheduling

import "errors"

// NodePoolLimitError indicates that a NodePool has insufficient remaining capacity.
type NodePoolLimitError struct {
	error
}

func NewNodePoolLimitError(err error) NodePoolLimitError {
	return NodePoolLimitError{error: err}
}

func IsNodePoolLimitError(err error) bool {
	e := &NodePoolLimitError{}
	return errors.As(err, e)
}

func (e NodePoolLimitError) Unwrap() error {
	return e.error
}

// IncompatibleRequirementsError indicates that a pod and NodeClaim have incompatible requirements.
type IncompatibleRequirementsError struct {
	error
}

func NewIncompatibleRequirementsError(err error) IncompatibleRequirementsError {
	return IncompatibleRequirementsError{error: err}
}

func IsIncompatibleRequirementsError(err error) bool {
	e := &IncompatibleRequirementsError{}
	return errors.As(err, e)
}

func (e IncompatibleRequirementsError) Unwrap() error {
	return e.error
}

// OfferingError indicates that compatible offerings could not be reserved.
type OfferingError struct {
	error
}

func NewOfferingError(err error) OfferingError {
	return OfferingError{error: err}
}

func IsOfferingError(err error) bool {
	e := &OfferingError{}
	return errors.As(err, e)
}

func (e OfferingError) Unwrap() error {
	return e.error
}

// VolumeConstraintError indicates that volume limits or topology prevent scheduling.
type VolumeConstraintError struct {
	error
}

func NewVolumeConstraintError(err error) VolumeConstraintError {
	return VolumeConstraintError{error: err}
}

func IsVolumeConstraintError(err error) bool {
	e := &VolumeConstraintError{}
	return errors.As(err, e)
}

func (e VolumeConstraintError) Unwrap() error {
	return e.error
}

// TaintTolerationError indicates that a pod does not tolerate a NodeClaim taint.
type TaintTolerationError struct {
	error
}

func NewTaintTolerationError(err error) TaintTolerationError {
	return TaintTolerationError{error: err}
}

func IsTaintTolerationError(err error) bool {
	e := &TaintTolerationError{}
	return errors.As(err, e)
}

func (e TaintTolerationError) Unwrap() error {
	return e.error
}
