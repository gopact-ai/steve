package httpapi

import "github.com/gopact-ai/steve/internal/consoleapi"

func (s *Server) SetAbandons(control consoleapi.Abandons) { s.abandons = control }
