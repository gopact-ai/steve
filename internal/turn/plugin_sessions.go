package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
)

type pluginRuntimeCloser interface {
	ClosePluginRuntime(context.Context, plugins.RuntimeRef) error
}

// ForgetPluginRuntime is called only after the management service has checked
// execution references and fenced new preparation. Conversation text lives in
// the console store; this releases only resumable native session identities.
func (c *Coordinator) ForgetPluginRuntime(ctx context.Context, ref plugins.RuntimeRef, confirm func(context.Context) error) error {
	release, err := c.SealIdle()
	if err != nil {
		return err
	}
	defer release()
	if closer, ok := c.runtime.(pluginRuntimeCloser); ok {
		if err := closer.ClosePluginRuntime(ctx, ref); err != nil {
			return err
		}
	} else {
		return errors.New("plugin runtime close is unavailable")
	}
	if confirm != nil {
		if err := confirm(ctx); err != nil {
			return err
		}
	}
	return c.store.ForgetPluginRuntime(ref.ID)
}

var _ pluginRuntimeCloser = (*harness.Manager)(nil)
