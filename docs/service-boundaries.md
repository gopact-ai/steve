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

## Identity authority

`identity.Owners` resolves the Console baseline and explicitly registered native
channel owners from the existing configured authority. The composition root uses
`config.EffectiveOwnerID()` (including its legacy fallback) and enabled adapters;
the coordinator and scheduler consume independent immutable snapshots of that
same configuration. No second owner file or automatic cross-channel account
binding is introduced. Request-local views carry `ownerID`, not a provider-named
identity, and unknown channels are refused before execution.

Native actor IDs on persisted tasks, attempts and grants remain intact. Existing
project grants are still keyed by native principal strings; adding shared
principal mappings requires an explicit grant migration rather than guessing that
equal ID strings in different providers identify the same person.

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

## Durable service ingress and intelligent Work

`internal/ingress` records immutable kind/source/key + authenticated actor,
conversation, canonical payload digest and the adapter's receipt. Console admission
and Feishu's existing input-command acceptance write that record in their own
ledger transaction. Imported history is not emitted as fresh input. Console and
Gateway retain their own execution queues and external delivery recovery.

`internal/work` is the provider/automation intelligence port. An owner-created
connection grant names exact Console conversation/project pairs. Work rejects
platform control commands, checks the existing binding and rereads live authority
at admission. The Work receipt and original Console Exchange ID commit in the
same transaction as queue admission. Final results are retained on the Work record
in the transaction committing the Console reply, so transcript deletion cannot
silently replace an accepted task with a new ACP turn. Unfinished work whose
original exchange cannot be found is reported as unknown.

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
guard. Native channel owners, project grants and retained actor IDs remain in their
existing namespaces; no implicit cross-channel principal migration is performed.

`clientcap` separates advertised availability from permission. Explicit grants
bind a capability to a conversation. Registration has a process generation and
45-second heartbeat; claims have stable invocation IDs and unique tokens.
Optional approval appears in the Inbox. Interrupted native effects become unknown
and are never automatically replayed. The original claim token can submit a late
receipt after server restart; a replacement claim cannot adopt that proof.

The ACP bridge offers capability discovery, invocation and result tools only to a
private Console execution. Admission and dispatch both check its original task,
attempt, process generation and live lease. Delegated/shared channel sessions do
not inherit the private client's authority. No arbitrary code-execution capability
is implicitly registered.

## Remaining product extensions

This is a modular server foundation, with existing Console/Gateway execution
adapters behind the service ports. Native mail/calendar/IM providers and macOS/iOS
capability implementations can use the versioned HTTP contract in
[event-providers](event-providers.md). They are not installed by this change.
Existing Skills/MCP immutable runtime bundles and user-managed harness versions
continue unchanged. A2A adapters and explicit cross-channel account/grant migration
are separate extensions requiring their own contracts and compatibility evidence.

## Validation cadence

Finish a coherent architecture batch before running the full suites and real-model
fleet gate. New tests must name a behavioral requirement or a concrete regression,
and assert an observable failure; call-count and coverage increases alone do not
establish effectiveness. Reuse existing authorization, recovery and queue tests.
After a failure or a later change, rerun the affected checks; final CI and required
fleet evidence refer to the final runtime commit.
