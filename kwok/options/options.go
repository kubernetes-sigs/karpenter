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

package options

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"
)

func init() {
	options.Injectables = append(options.Injectables, &Options{})
}

type optionsKey struct{}

// Options contains all CLI flags / env vars for the KWOK cloudprovider.
type Options struct {
	InstanceTypesFilePath string
}

func (o *Options) AddFlags(fs *options.FlagSet) {
	fs.StringVar(&o.InstanceTypesFilePath, "instance-types-file-path", env.WithDefaultString("INSTANCE_TYPES_FILE_PATH", ""), "Path to a custom instance-types file")
}

func (o *Options) Parse(fs *options.FlagSet, args ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return fmt.Errorf("parsing flags, %w", err)
	}
	return nil
}

// FromArgs returns the options set by args and the environment, ignoring every flag that isn't one of these options,
// so that the kwok binary can read them before the operator parses its flags.
func FromArgs(args []string) *Options {
	o := &Options{}
	fs := &options.FlagSet{FlagSet: flag.NewFlagSet("kwok", flag.ContinueOnError)}
	fs.SetOutput(io.Discard)
	o.AddFlags(fs)
	for len(args) > 0 {
		// Parse stops at an unknown flag, which it usually consumes, or before a non-flag argument, such as an unknown
		// flag's value. Skip the argument it stopped at if it didn't consume one, and resume after it.
		before := len(args)
		_ = fs.Parse(args)
		if args = fs.Args(); len(args) == before {
			args = args[1:]
		}
	}
	return o
}

func (o *Options) ToContext(ctx context.Context) context.Context {
	return ToContext(ctx, o)
}

func ToContext(ctx context.Context, opts *Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

func FromContext(ctx context.Context) *Options {
	retval := ctx.Value(optionsKey{})
	if retval == nil {
		return nil
	}
	return retval.(*Options)
}
