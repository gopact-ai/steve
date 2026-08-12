package feishu

import (
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestNormalizeTextMessage(t *testing.T) {
	event := messageEvent("user", "text", `{"text":"@_user_1 hello"}`)

	msg, ok := normalize(event)
	if !ok {
		t.Fatal("normalize rejected a text message")
	}
	if msg.ChatID != "oc_chat" || msg.MessageID != "om_message" || msg.Text != "hello" {
		t.Fatalf("unexpected message: %#v", msg)
	}
}

func TestNormalizeIgnoresBotMessages(t *testing.T) {
	if _, ok := normalize(messageEvent("bot", "text", `{"text":"loop"}`)); ok {
		t.Fatal("normalize accepted a bot message")
	}
}

func messageEvent(senderType, messageType, content string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{
			SenderType: &senderType,
			SenderId:   &larkim.UserId{OpenId: ptr("ou_sender")},
		},
		Message: &larkim.EventMessage{
			ChatId:      ptr("oc_chat"),
			ChatType:    ptr("group"),
			MessageId:   ptr("om_message"),
			MessageType: &messageType,
			Content:     &content,
		},
	}}
}

func ptr[T any](value T) *T { return &value }
