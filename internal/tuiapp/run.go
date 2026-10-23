package tuiapp

import (
	"context"
	"io"

	tea "charm.land/bubbletea/v2"
)

// Run starts the client on the terminal behind input and output and
// returns when the user quits. The event stream is followed for the life
// of the program and resumes after its last cursor when the server comes
// back.
func Run(ctx context.Context, api API, opts Options, input io.Reader, output io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	program := tea.NewProgram(New(ctx, api, opts), tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output))
	updates := api.Follow(ctx, "", "")
	go func() {
		for u := range updates {
			program.Send(StreamMsg(u))
		}
	}()
	_, err := program.Run()
	if ctx.Err() != nil && err != nil {
		// Quitting cancels the context; that is not a failure.
		return nil
	}
	return err
}
