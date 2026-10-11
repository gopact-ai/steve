# Service boundary migration

Steve remains a modular Go server shared by its clients. ACP invokes an existing,
user-managed harness; service boundary work does not introduce a new harness or
microservices.

## Schedule handoff

`internal/schedule.ScheduleReceiver` receives a frozen firing and returns a
channel-defined receipt. `ReceiverRegistry` binds names explicitly; the application
composition root supplies a shared task-channel registry implementing this port.
Console and Gateway are registered once for schedule and task handoffs.
Additional channels register receivers without editing the dispatcher. Adapters delegate to the
existing receivers and preserve their authorization and admission checks.

The handoff does not collapse these distinct stages:

- Console returns an Exchange ID after durable admission; execution can continue.
- Gateway handles the scheduled input before returning its announcement Message ID.
- External delivery can remain unknown even if execution has completed.
- The schedule ledger records acceptance separately from the receiver's work.

The dispatcher retains BeginFiring, AcceptFiring, FailFiring, task rotation and
empty-receipt handling. The store retains the existing conservative recovery
policy: Console retries the same durable firing key and finds the original
exchange; every other channel's interrupted dispatch becomes unknown. Registering
a receiver grants no replay capability. A future explicit capability must be
supported by evidence of durable deduplication before broadening that policy.

## Turn input contract

`turn.Request` separates four concerns used by Console, Gateway, onboarding and
every retained-turn recovery entry:

- `Source` records channel, conversation, input message, chat context and origin.
- `Actor.ID` is the adapter-authenticated native caller. Owner and project checks
  still resolve that ID in the source channel; IDs are not rewritten across channels.
- `ReplyContext` carries the adapter's chat and open-card metadata. The current
  reply route remains the source address. Arbitrary cross-channel replies require
  a separate authorization and delivery design.
- `Admission` carries the exact Console exchange, project/task fences, resume
  incarnation and queue policy. These remain preconditions checked by execution,
  not authority conferred by source or delivery metadata.

The request and its callbacks are in-process contracts. HTTP input bodies,
durable Console exchanges, Gateway inputs, task records and attempt identities
retain their existing formats; they are not serialized as `turn.Request`.
`Handle` and retained-turn operations keep their signatures and existing
authorization, original-attempt recovery, interrupt and maintenance behavior.
Existing internal Go callers use the explicit components, with no parallel legacy
fields or automatic identity conversions. A registered provider test exercises
the same coordinator through task and attempt admission; it is not a mail adapter.

Console and Gateway both consume `turn.InputExecutor`, the shared execution and
control-parsing port. Their durable acceptance, transcript, progress callbacks and
external reply reconciliation remain adapter responsibilities. This is a common
execution entrance, not a claim that two different durable queues are one store.

## Conversation, Work, Execution and Interaction

`conversation.Controller` owns agent selection, activity and arrival context.
`conversation/store` owns the durable conversation documents, exchanges, queue,
questions and recovery previously owned by Console. `console` preserves the public
Go names as a compatibility facade. Existing `console-*` ledger kinds and JSON
images stay intact; moving a module does not re-admit historical messages.

`turnapi` is the shared input, context, recovery and control-syntax contract.
Conversation storage and Gateway consume these contracts without importing the
Coordinator implementation. Work owns dispatch selection and the common input
receipt. Coordinator supplies existing platform commands and native execution.
`execution`, `lifecycle`, `task` and `attempt` continue to own execution epochs,
leases, accounting and the original harness session. No second executor is added.

`work.Engine` records the authenticated native account and immutable input digest
before dispatch. Admission binds its original Task/Attempt; known completion and
`work.completed` outbox commit together. Replaying a settled receipt returns its
original result. Running or uncertain execution requires original-attempt recovery
and never silently creates a replacement attempt.

`interaction.Service` records execution-bound questions and decisions for chat,
delegated and auxiliary plan executions. Presenters authenticate/render the reply;
the service rechecks the original execution, account revision and project grant.
A restarted/expired presenter cannot approve work. Native questions keep their
presenter's timeout policy, including waits across a node outage. Existing
recovery questions and outbound delivery receipts retain their separate semantics:
retrying observation is not permission to repeat an uncertain native effect.

## Identity authority

An authenticated `identity.Account` is `(channel, native ID)`. Its default user
principal is a stable digest of the tuple, so equal strings across providers do not
share authority. Configured native owners explicitly link to the Console owner's
principal. Removing that declaration retires the link; an unchanged restart does
not change its revision. Owner administration can link/unlink/disable other
accounts with a revision fence and an audit record. Provider/client connection
tokens remain scoped delegations and cannot become native accounts or owners.

Project grants use canonical user principals. Startup migrates both runtime and
configuration grants from the explicitly selected legacy namespace (Feishu when
enabled, otherwise Console), retaining the original grant/account in an audit.
New grants accept canonical `user:<digest>` or `account:<channel>:<native ID>`;
chat commands resolve an unqualified target in their actual source channel.
Conflicting aliases produce a deny grant. Disabled account metadata can still be
reconciled, while execution remains denied. Task/Attempt native sender fields and
original receipts are never rewritten by this migration.

Every new execution retains the original account revision. MCP calls recheck that
proof and the current project policy, including a delegated child's exact parent
turn. Unlink, disable or permission removal cannot be bypassed by an old session
or token. Auxiliary questions inherit the creating task's frozen caller proof;
missing/ambiguous legacy authority stays refused rather than adopting the owner.

Owner HTTP endpoints expose `/console/accounts` (GET/PUT),
`/console/grant-migrations` and `/console/work-interactions`. They use the existing
owner guard; connection credentials cannot access them.

## Task-channel handoffs

One registry in the composition root owns schedule, task-notice, manual-resume,
accepted-resume dispatch and child-result handoffs. Routing selects only the
persisted channel. Console-looking conversation or message IDs cannot redirect
another provider's work. Registering a third receiver requires no switch in
delegation, task routing or the schedule dispatcher.

The receiver declares continuation replay safety and owns receipt lookup.
Console keeps stable-key durable continuation receipts and deferred processing;
Gateway keeps its asynchronous confirmation callback and conservative external
delivery semantics. Unknown channels supply no receipt evidence, and registering
a receiver does not change the schedule store's interrupted-delivery policy.

## Native channel boundary and controlled Work

Gateway imports normalized `channel.Input`, `channel.Action`, semantic `view.Turn`
and `turnapi` contracts. Feishu authentication, enrichment and card rendering live
in its adapter. A plain `channel.Adapter` needs receipt-bearing text messaging and
enrichment; unsupported cards/topics/reactions return `channel.ErrUnsupported`.
A rich adapter supplies `channel.ConversationTransport` and `channel.Presenter`.

Installed server integrations register `channel.Provider` by name through
`app.Config.Adapters`. Their lifecycle receives authenticated-input/action handlers
and a readiness callback. Composition registers each adapter for schedules, task
notices, resumes, child results and completion recovery. This is a Go embedding
port; the default executable does not dynamically load arbitrary shared libraries.
External event providers continue to use the scoped HTTP protocol.

Admission commits a conversation route and its native message anchor. Historical
routes are restored from actual persisted heads/admission/topic receipts, never
from a conversation-ID prefix. Providers must supply stable, globally unique
conversation identities; collisions are refused instead of merging conversations.
Plain replies preserve known provider receipts and the original conversation.

Controlled Work can target an enrolled conversation/project on any registered
channel. Console returns its original Exchange. A native private owner conversation
returns its original Gateway admission and requires an authenticated owner anchor.
The provider cannot invent that anchor or submit platform controls. Automation
announcements have their own immutable native receipt; an unknown announcement
stops before ACP and publishes a review event to the fallback Inbox. An unknown
final send retains the completed Work and its original delivery proof separately.

`work.Routes` selects the persisted channel. Work acceptance and the original
adapter receipt commit in one transaction. Results retain the original Task/Attempt
and known output even after transcript deletion. Transport-specific acceptance and
external delivery reconciliation remain adapter responsibilities; neither an
accepted queue entry nor a completed execution proves external delivery.

## Business events, subscriptions and fallback Inbox

`internal/events` owns a durable business log, transactional outbox, frozen
subscription revisions, consumer claims and processing receipts. `servicehub`
composes identity, events, Work and client capabilities and owns their bounded
recovery pump. It contains no harness or transport implementation. HTTP consumes
`serviceapi` ports; ACP continues through the existing execution system.

Subscriptions route events to Inbox, intelligent Work, or an enrolled provider.
Unmatched external events and failed/uncertain processing remain visible. Owners
can inspect an event, add a subscription and reprocess it. Existing deliveries
under the same revision retain their receipts; changing a rule then reprocessing
is an explicit decision to create a delivery under the new revision.

Derived events require opt-in; recorded causation is bounded at eight levels.
Terminal facts still commit at the limit, but effect subscriptions stop and leave
an Inbox entry. Ordinary completion facts do not flood the Inbox. SSE remains a
read-model feed and is never a reliable business consumer.

## Connection identity and client capabilities

`identity.Service` persists scoped, revocable provider/client delegations. Only
credential digests are stored. Connection tokens never pass the Console owner
guard. Native account mappings and project policy are distinct from connection grants;
retained actor IDs remain in their original namespaces.

`clientcap` separates advertised availability from permission. Explicit grants
bind a capability to a conversation. Registration has a process generation and
45-second heartbeat; claims have stable invocation IDs and unique tokens.
Optional approval appears in the Inbox. Interrupted native effects become unknown
and are never automatically replayed. The original claim token can submit a late
receipt after server restart; a replacement claim cannot adopt that proof.

The ACP bridge offers capability discovery, invocation and result tools only to a
private owner execution on its recorded native channel. Admission and dispatch both check its original task,
attempt, process generation and live lease. Delegated/shared sessions do
not inherit the private client's authority. No arbitrary code-execution capability
is implicitly registered.

## Remaining product extensions

This is a modular server foundation, with existing Console/Gateway execution
adapters behind the service ports. Native mail/calendar/IM providers and macOS/iOS
capability implementations can use the versioned HTTP contract in
[event-providers](event-providers.md). They are not installed by this change.
Existing Skills/MCP immutable runtime bundles and user-managed harness versions
continue unchanged. A2A adapters remain a product extension. Account/grant migration is part of the
foundation above, with explicit bindings and original execution evidence.

## Validation cadence

Finish a coherent architecture batch before running the full suites and real-model
fleet gate. New tests must name a behavioral requirement or a concrete regression,
and assert an observable failure; call-count and coverage increases alone do not
establish effectiveness. Reuse existing authorization, recovery and queue tests.
After a failure or a later change, rerun the affected checks; final CI and required
fleet evidence refer to the final runtime commit.
