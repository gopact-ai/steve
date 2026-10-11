// Package console is the compatibility facade for the web channel. Durable
// conversation documents, queues and recovery are owned by conversation/store.
// Keeping these aliases preserves callers and the on-disk console record image.
package console

import (
	"github.com/gopact-ai/steve/internal/conversation/store"
)

type Service = store.Service
type Coordinator = store.Coordinator
type Events = store.Events
type Meta = store.Meta
type Exchange = store.Exchange
type QuoteRef = store.QuoteRef
type Titler = store.Titler
type Inspector = store.Inspector
type MaterialResolver = store.MaterialResolver
type RetainedChatDriver = store.RetainedChatDriver
type DurableExchange = store.DurableExchange
type DurableState = store.DurableState
type TransferExchange = store.TransferExchange
type ProjectTransfer = store.ProjectTransfer
type RecoveryConversation = store.RecoveryConversation
type CompletionRead = store.CompletionRead
type MessageSender = store.MessageSender

const Prefix = store.Prefix
const ChatID = store.ChatID
const AnchorMark = store.AnchorMark

var New = store.New
var ConversationID = store.ConversationID
var IsConsole = store.IsConsole
var NativeImportConversation = store.NativeImportConversation
var CheckTaskCompletionTx = store.CheckTaskCompletionTx
var ReadCompletion = store.ReadCompletion
var LoadState = store.LoadState
var StoreStateTx = store.StoreStateTx
var ConfirmedAttemptDeliveryTx = store.ConfirmedAttemptDeliveryTx
var ExportProject = store.ExportProject
var ExportProjectDocument = store.ExportProjectDocument
var ImportProjectDocument = store.ImportProjectDocument
var ValidateProjectImportTx = store.ValidateProjectImportTx
var ImportProjectTx = store.ImportProjectTx
var FreezeProjectDocument = store.FreezeProjectDocument
var FreezeProjectTx = store.FreezeProjectTx
