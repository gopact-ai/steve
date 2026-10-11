package httpapi

import "net/http"

func (s *Server) assistantDelivery(w http.ResponseWriter, r *http.Request) {
	if !s.assistantEnabled(w) {
		return
	}
	out, err := s.assistant.Events.Delivery(r.Context(), r.PathValue("id"))
	assistantResponse(w, out, err)
}
