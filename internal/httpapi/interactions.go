package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (s *Server) consoleQuestions(w http.ResponseWriter, r *http.Request) {
	service, ok := s.console.(consoleapi.Interactions)
	if !ok {
		http.Error(w, "console interactions are not enabled", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"questions": service.Questions(r.URL.Query().Get("conversation"))})
}

func (s *Server) consoleAnswer(w http.ResponseWriter, r *http.Request) {
	service, ok := s.console.(consoleapi.Interactions)
	if !ok {
		http.Error(w, "console interactions are not enabled", http.StatusNotImplemented)
		return
	}
	var answer consoleapi.QuestionAnswer
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		http.Error(w, "one answer is required", http.StatusBadRequest)
		return
	}
	question, err := service.AnswerQuestion(r.Context(), r.PathValue("id"), answer)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, consoleapi.ErrQuestionNotFound):
			status = http.StatusNotFound
		case errors.Is(err, consoleapi.ErrQuestionConflict):
			status = http.StatusConflict
		case errors.Is(err, consoleapi.ErrQuestionForbidden):
			status = http.StatusForbidden
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"question": question})
}
