package httpapi

import (
	"net/http"

	"github.com/gopact-ai/steve/internal/clientapi"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// clientBackend lends steve.client.v1 the services this server is wired
// with, read at each request as the console routes read them.
type clientBackend struct{ s *Server }

func (b clientBackend) Model() clientapi.Model {
	if b.s.model == nil {
		return nil
	}
	return b.s.model
}
func (b clientBackend) Console() consoleapi.Console { return b.s.console }
func (b clientBackend) Attempts() clientapi.Attempts {
	if b.s.admin == nil {
		return nil
	}
	return b.s.admin
}
func (b clientBackend) ChannelHistory() consoleapi.ChannelHistory { return b.s.channelHistory }
func (b clientBackend) ServerVersion() string                     { return nodewire.Version() }

// clientRoutes serves the versioned client protocol beside the console,
// behind the same token.
func (s *Server) clientRoutes(mux *http.ServeMux) {
	clientapi.New(clientBackend{s}).Routes(mux, s.guard)
}
