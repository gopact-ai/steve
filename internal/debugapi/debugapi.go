// Package debugapi exposes a loopback-only HTTP endpoint that injects inbound
// messages and card callbacks. Card buttons can only be pressed from a Feishu
// client, so this is the one way to exercise the approval, stop and retry paths
// end to end without a human tapping the card.
package debugapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/protocol"
)

type Gateway interface {
	HandleMessage(feishu.InboundMessage)
	HandleCardAction(feishu.CardAction) feishu.CardToast
	LiveTurns() []gateway.TurnInfo
	LastCard() []byte
}

// Sender posts the injected prompt into the chat for real. Cards reply to a
// message, so an injected turn needs a message that actually exists.
type Sender interface {
	SendChat(ctx context.Context, chatID, text string) (feishu.Sent, error)
}

// Defaults fill in the fields a caller usually does not care about, so a test
// prompt is just {"text": "..."}.
type Defaults struct {
	ChatID       string
	SenderOpenID string
	Sender       Sender
}

func Serve(ctx context.Context, addr string, gw Gateway, def Defaults) error {
	if err := checkLoopback(addr); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           Handler(gw, def),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info(fmt.Sprintf("debugapi: listening on %s", addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("debug endpoint: %w", err)
	}
	return nil
}

func Handler(gw Gateway, def Defaults) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /message", func(w http.ResponseWriter, r *http.Request) {
		var req messageRequest
		if !decode(w, r, &req) {
			return
		}
		msg := req.inbound(def)
		if msg.ChatID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "chat_id is required"})
			return
		}
		if req.MessageID == "" && def.Sender != nil {
			sent, err := def.Sender.SendChat(r.Context(), msg.ChatID, req.Text)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
				return
			}
			msg.MessageID, msg.ChatID = sent.MessageID, sent.ChatID
		}
		go gw.HandleMessage(msg)
		writeJSON(w, http.StatusAccepted, map[string]any{"message_id": msg.MessageID})
	})
	mux.HandleFunc("GET /turns", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"turns": gw.LiveTurns()})
	})
	mux.HandleFunc("GET /card", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if payload := gw.LastCard(); payload != nil {
			_, _ = w.Write(payload)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /card", func(w http.ResponseWriter, r *http.Request) {
		var req cardRequest
		if !decode(w, r, &req) {
			return
		}
		toast := gw.HandleCardAction(req.action(def))
		writeJSON(w, http.StatusOK, map[string]any{"toast_type": toast.Type, "toast": toast.Content})
	})
	return mux
}

type messageRequest struct {
	Text         string `json:"text"`
	Quote        string `json:"quote"`
	ChatID       string `json:"chat_id"`
	ChatType     string `json:"chat_type"`
	MessageID    string `json:"message_id"`
	SenderOpenID string `json:"sender_open_id"`
	Mentioned    *bool  `json:"mentioned"`
}

func (r messageRequest) inbound(def Defaults) feishu.InboundMessage {
	msg := feishu.InboundMessage{
		ChatID:       or(r.ChatID, def.ChatID),
		MessageID:    or(r.MessageID, fmt.Sprintf("om_debug_%d", time.Now().UnixNano())),
		SenderOpenID: or(r.SenderOpenID, def.SenderOpenID),
		Text:         r.Text,
		Quote:        r.Quote,
		ChatType:     protocol.ParseChatType(or(r.ChatType, string(protocol.ChatGroup))),
		Mentioned:    true,
	}
	if r.Mentioned != nil {
		msg.Mentioned = *r.Mentioned
	}
	return msg
}

type cardRequest struct {
	Action    string `json:"action"`
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
	OpenID    string `json:"open_id"`
	MessageID string `json:"message_id"`
}

func (r cardRequest) action(def Defaults) feishu.CardAction {
	return feishu.CardAction{
		Action:    r.Action,
		RequestID: r.RequestID,
		Decision:  r.Decision,
		OpenID:    or(r.OpenID, def.SenderOpenID),
		MessageID: r.MessageID,
	}
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func or(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// checkLoopback refuses to expose the injection endpoint beyond this machine.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("debug endpoint address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("debug endpoint address %q must be loopback", addr)
	}
	return nil
}
