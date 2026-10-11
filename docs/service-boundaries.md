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

## Next boundaries

1. Add explicit cross-channel principal bindings only with a reviewed migration
   of legacy project grants and retained work identities.
2. Evaluate a shared durable work entrance using the existing ledger and stable
   IDs, without prematurely merging Console's queue with Gateway's dispatch,
   reply and suppression responsibilities.
3. Add durable business events, subscriptions, provider consumption results and
   a fallback Inbox. SSE remains a frontend read-model feed.

Across these steps, retain command key + actor + payload identity, recovery of
original attempts for uncertain dispatch, conversation serialization, cancel and
steer, maintenance and retirement fencing, leases and stop proofs. Coordinator
responsibilities can then be narrowed across Conversation, Work, Execution,
Interaction and Automation. The current schedule change does not alter queues,
identity storage or external delivery recovery. Task routing now uses explicit
receiver registration while retaining the original adapter semantics.

## Validation cadence

Finish a coherent architecture batch before running the full suites and real-model
fleet gate. New tests must name a behavioral requirement or a concrete regression,
and assert an observable failure; call-count and coverage increases alone do not
establish effectiveness. Reuse existing authorization, recovery and queue tests.
After a failure or a later change, rerun the affected checks; final CI and required
fleet evidence refer to the final runtime commit.
