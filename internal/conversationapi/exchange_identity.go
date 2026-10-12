package conversationapi

type ExchangeIdentity interface {
	ExchangeConversation(id string) (string, bool)
}
