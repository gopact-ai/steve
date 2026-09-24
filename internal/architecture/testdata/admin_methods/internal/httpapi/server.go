package httpapi

import "github.com/gopact-ai/steve/internal/consoleapi"

type Server struct{ admin consoleapi.Admin }

func (s *Server) handler() func() {
	s.admin.Called()
	return s.admin.Referenced
}
