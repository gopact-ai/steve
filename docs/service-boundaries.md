# Service boundary migration

Steve remains a modular Go server shared by its clients. ACP invokes an existing,
user-managed harness; service boundary work does not introduce a new harness or
microservices.

## Schedule handoff

`internal/schedule.ScheduleReceiver` receives a frozen firing and returns a
channel-defined receipt. `ReceiverRegistry` binds names explicitly; the application
composition root registers Console and Gateway adapters. Additional channels
register receivers without editing the dispatcher. Adapters delegate to the
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

## Next boundaries

1. Introduce a neutral identity service using existing identity and configuration
   authority, removing Console's dependency on `feishu.owner_open_id`.
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
identity storage, external delivery recovery or task transport routing.
