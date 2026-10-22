# Service boundaries

Steve's service modules share one Go application and ledger. They expose contracts for conversations, events and authorized work; ACP execution remains owned by the task and execution modules.

## Input and conversations

`turnapi` defines input, context, recovery and control syntax. A turn separates four concerns:

| Field | Meaning |
| --- | --- |
| `Source` | Channel, conversation, message, chat context and origin |
| `Actor.ID` | Native caller authenticated by the adapter |
| `ReplyContext` | Original adapter reply route and presentation metadata |
| `Admission` | Exchange identity, project/task fences, recovery incarnation and queue policy |

Source and presentation metadata confer no execution authority. Console and Gateway consume the input executor contract; adapters own durable intake, transcript presentation and external delivery receipts.

`conversation.Controller` owns selection and arrival context. Conversation storage owns exchanges, queues, questions and recovery records; `console` provides compatible Go entry points. Stored messages are not serialized `turn.Request` values.

`conversationapi` holds transport-neutral presentation data. Plan and delegation observations attach to their original recorded exchange, never to a later message when original history is missing.

## Work and execution

Work stores the authenticated caller, immutable input digest and original input receipt. Admission binds the original Task/Attempt. Completion and its `work.completed` event commit together; replay returns the original result.

An accepted queue entry is not a completed execution, and completed execution is not proof of external message delivery. Running or uncertain work uses original-attempt recovery. A retry cannot silently create another attempt or external effect.

`execution`, `lifecycle`, `task` and `attempt` own epochs, leases, budgets, process lifetime and result commits. `interaction.Service` binds questions and decisions to the original execution, account revision and project grant. Presenters authenticate and render answers; expired presenters cannot approve new work.

## Identity and controls

An account is `(channel, native ID)`. Canonical user principals derive from this pair; matching strings in different channels do not share permissions. Explicit account links connect identities to the owner or another principal, with revisions and audit records.

Project grants use canonical principals. Legacy grants are reconciled from the declared native namespace with original evidence retained; conflicting aliases deny access. New grants accept `user:<digest>` or `account:<channel>:<native ID>`. Native sender fields remain unchanged.

Executions retain their original account revision. MCP and platform controls recheck it alongside current project policy. Delegated children retain parent authority, not a new owner identity. Fleet administration requires the original private owner execution. Account unlinking, disabling and grant removal affect new admissions and resumed access.

Owner endpoints include `/console/accounts`, `/console/grant-migrations` and `/console/work-interactions`. Provider/client credentials cannot access these owner endpoints.

## Channels and schedules

Native adapters normalize inputs, actions and semantic presentation. Feishu owns its authentication, enrichment and cards. Plain adapters supply receipt-bearing text messaging; richer adapters implement conversation transport and presentation. Go embeddings register `channel.Provider` through `app.Config.Adapters`; the executable does not dynamically load arbitrary libraries.

One registry routes schedules, notices, resumes and child results by the persisted channel. Conversation prefixes never select authority or delivery routes. Providers supply stable unique conversation IDs; collisions are refused.

A scheduled firing has a frozen identity and a channel receipt. Console can recover the same durable exchange; interrupted external sends without replay evidence remain unknown. Receiver registration alone does not grant replay safety. Native Work needs an authorized owner conversation anchor and retains announcement and final-delivery receipts separately.

## Events and subscriptions

`events` owns immutable business events, transactional outbox records, frozen subscription revisions, claims and receipts. `servicehub` composes recovery loops; `serviceapi` exposes narrow HTTP ports.

Subscriptions target Inbox, Work or an enrolled provider. Unmatched external events and failed or uncertain processing appear in Inbox. Reprocessing preserves deliveries for existing revisions; a new rule revision can create a new delivery. Derived events require opt-in, with a maximum causal depth of eight before effects stop and attention is recorded.

SSE is a UI feed, not a durable business subscription. Provider registration publishes metadata and health; it neither installs code nor grants execution. The [HTTP protocol](event-providers.md) defines authorization and receipt handling.

## Connections and client capabilities

Connections are scoped, revocable delegations with stable grant IDs. The server stores credential digests. Credential and scope revisions are independent; rotation preserves event sources, Work and registrations. Protected transactions check the credential generation, while accepted background work rechecks live grant scope.

Client capabilities have explicit conversation grants, process generations and heartbeat availability. Invocation records have stable IDs and claim tokens. Approval, when required, appears in Inbox. Dispatch rechecks the creating execution, epoch, process generation and lease; private owner capabilities are unavailable to delegated or shared sessions.

Clients validate their own input schemas and persist effect deduplication receipts. Missing completion proof becomes unknown, not automatically repeated. A late original claim can submit evidence after restart; a new generation does not prove the old effect stopped.

## History and capacity

External intake defaults to 256 unfinished records per source and 4096 per service. Accepted idempotent retries return original receipts even at capacity; rejected new intake receives HTTP 429 and `Retry-After`. Completion facts are recorded independently of external intake.

Eligible terminal payloads older than seven days can be compressed losslessly within the ledger. Typed reads verify and decode them. Active grants, subscriptions, routes and unresolved effects remain; compression does not impose a destructive history TTL or a fixed total storage size.

`GET /console/service-backlog` reports counts, oldest timestamps, limits and storage. `POST /console/service-history/compact` requests a bounded compaction pass. Owners can cancel unclaimed client calls or reconcile unknown effects with a revision and evidence:

- `POST /console/client-invocations/{id}/cancel`
- `POST /console/client-invocations/{id}/reconcile`
- `POST /console/event-deliveries/{id}/reconcile`

Reconciliation records evidence without dispatching another action. Project export does not transfer service connections or their credentials to another Hub.
