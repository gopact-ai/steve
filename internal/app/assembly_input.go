package app

import (
	"context"
)

// Assembly interfaces carry completed construction results between subsystem
// factories. Concrete constructors remain inside the composition package.
type inputAssembly interface {
	Parent() context.Context
	ConfigPath() *string
	Environment() *Environment
}
type assemblyInput struct {
	parent      context.Context
	path        string
	environment *Environment
}

func (v *assemblyInput) Parent() context.Context   { return v.parent }
func (v *assemblyInput) ConfigPath() *string       { return &v.path }
func (v *assemblyInput) Environment() *Environment { return v.environment }

type channelRuntime interface{ SetRuntimeError(string) }
