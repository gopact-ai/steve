package app

import (
	"context"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

// Assembly interfaces carry completed construction results between subsystem
// factories. Concrete constructors remain inside the composition package.
type inputAssembly interface {
	Adapters() map[string]channel.Provider
	Parent() context.Context
	ConfigPath() *string
	Environment() *Environment
}
type assemblyInput struct {
	adapters    map[string]channel.Provider
	parent      context.Context
	path        string
	environment *Environment
}

func (v *assemblyInput) Parent() context.Context               { return v.parent }
func (v *assemblyInput) ConfigPath() *string                   { return &v.path }
func (v *assemblyInput) Environment() *Environment             { return v.environment }
func (v *assemblyInput) Adapters() map[string]channel.Provider { return v.adapters }

type channelRuntime interface {
	SetRuntimeError(string)
	SetStartupRetry(*consoleapi.ChannelStartupRetry)
	SetReconnect(*consoleapi.ChannelReconnect)
	BindAccessUpdater(func(config.Feishu))
}
