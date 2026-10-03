# S−1 emulator spike — findings

Date 2026-10-03, framen. Throwaway code (not in the repo), run against the local emulator in `emulator/`.

- SDK: `github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus v1.10.0` (latest, 2025-08-05), its `admin` package, `azcore v1.23.2`, `github.com/Azure/go-amqp v1.4.0`. Go 1.27.1.
- Emulator: `mcr.microsoft.com/azure-messaging/servicebus-emulator:latest` + `mssql/server:2022-latest`, compose project `lazybus-emulator`.
- Connection string (AMQP, host port 5682):
  `Endpoint=sb://localhost:5682;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=SAS_KEY_VALUE;UseDevelopmentEmulator=true;`
- Admin connection string (HTTP, host port 5310):
  `Endpoint=sb://localhost:5310;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=SAS_KEY_VALUE;UseDevelopmentEmulator=true;`

Summary: peek, send, complete, dead-letter markers and duplicate detection behave as the spec assumes. Four assumptions fail:

1. **The by-sequence scan can't abandon each non-matching message right away.** An abandoned message goes straight back to the head of the queue, and the next receive returns it again, so the scan never moves forward.
2. **On the emulator the admin client can't return runtime counts.** It lists entities, but every runtime-properties call errors, and the subscription ones panic.
3. **Peek on the active side of a sessionful queue fails.** A plain receiver gets `amqp:not-allowed`.
4. **The emulator's config can't hold a topic with zero subscriptions**, and its duplicate-detection window is capped at 5 minutes.

## 1. Peek from a sequence number on a DLQ

Works for both queue and subscription DLQs.

- API: `client.NewReceiverForQueue(q, &ReceiverOptions{SubQueue: SubQueueDeadLetter})` (or `NewReceiverForSubscription(topic, sub, …)`), then `receiver.PeekMessages(ctx, n, &PeekMessagesOptions{FromSequenceNumber: &seq})`.
- `PeekMessages(from=3, n=2)` on `orders/$DLQ` returned seq 3, 4. Peeking all 5 a second time showed `DeliveryCount` unchanged (1 on all).
- The subscription DLQ (`order-events/billing`) behaved the same.
- Peek from a sequence number that was removed (seq 12, completed) returns the **next higher** message (seq 13). That is what the §6 step 1 pre-check relies on: returned seq ≠ N → NotFound. Peek from beyond the end returns 0 messages.
- Sequence numbers are per entity. The DLQ keeps the source message's sequence number: messages dead-lettered from `orders` seq 11–15 show seq 11–15 in `orders/$DLQ`.
- Peeked messages carry a `LockedUntil` value (the `x-opt-locked-until` annotation, carried over from the original lock). It is meaningless for peek, so don't show or use it.
- A plain receiver on a **sessionful** queue's DLQ peeks fine (item 5). Peek on the sessionful queue's **active** side fails, see item 5.

## 2. Peek-lock receive on a DLQ, find by sequence number, abandon

**Go SDK has no prefetch setting.** `ReceiverOptions` has no `PrefetchCount`. The link is opened with manual credit (`Credit: -1`). Each `ReceiveMessages(ctx, max, opts)` issues `max − outstandingCredits` credits and returns as soon as it has `max` messages or `TimeAfterFirstMessage` (default 20 ms in peek-lock) has passed since the first message. Unused credits stay on the link. Between calls, a background "releaser" goroutine receives any message that arrives on those credits and **releases** it (AMQP `released` outcome). So "prefetch 0" in Go means: ask for exactly the batch size and accept that leftover credits can lock and release a few extra messages. Observed: `ReceiveMessages(10)` with 3 in the DLQ returned 3, and the 7 extra credits did no visible harm.

**Abandoning each non-matching message right away does not work** (spec §6 step 3.1 as written):

| Strategy | Setup | Result |
|---|---|---|
| A: receive batch of 2, abandon both before the next receive | 6 msgs in `invoices/$DLQ`, target = index 4 | **never found.** 30 receives returned only seq 1 and 2 (15× each). Abandon puts the message back at the head, and the next receive gets it again. |
| A, batch of 1 (item 1 run) | target = index 1 | head message seq 11 came back 1 465 times before seq 12 was delivered once. |
| B: receive batches of 2, **keep the locks** on non-matching messages until the target is found, then abandon all of them | same as A | found after 6 receives (each message once), 12 ms. |

So the scan must hold the locks on all non-matching messages it has received until the target is found (or the scan limit K is reached), then abandon all of them and await every abandon. The locks live for milliseconds and are all released before the repair returns, so ADR 0002 still holds.

DeliveryCount:

- **DLQ (emulator):** abandon does **not** increase `DeliveryCount`. After strategies A and B (and the 1 465 abandons), every DLQ message still had `DeliveryCount=1`. Whether Azure behaves the same is **not verified** (my guess: Azure does increase it, as on active queues). Keep the "DeliveryCount +1" warning until it is checked on a real namespace.
- **Active queue (emulator):** abandon increases it (1 → 2 → 3). With `MaxDeliveryCount=3`, the message went to the DLQ after the third abandon with `DeliveryCount=4`, `DeadLetterReason=MaxDeliveryCountExceeded`, `DeadLetterErrorDescription="Message could not be consumed after 3 delivery attempts."`.
- The SDK reports `DeliveryCount = header.delivery-count + 1`. A message received once and then dead-lettered shows `DeliveryCount=1` in the DLQ.

`LockedUntil`: present on received messages (`*time.Time`, now + LockDuration, e.g. 60 s on `invoices`). The §6 step 3.3 local lock check is possible. On a sessionful queue's DLQ, peek showed `LockedUntil = 10000-01-01` (sentinel), so treat absurd values as "unknown".

## 3. Send a copy, complete the original, markers

Worked end to end: `orders/$DLQ` seq 11 → copy sent to `orders` (new seq 18) → `CompleteMessage` → DLQ count −1. The copy had no markers.

Where the markers live in `ReceivedMessage` (SDK `message.go` `newReceivedMessage`):

- `DeadLetterReason` and `DeadLetterErrorDescription` are **application properties**. They appear in `ApplicationProperties` (string values) **and** the SDK copies them into the convenience fields `ReceivedMessage.DeadLetterReason` / `.DeadLetterErrorDescription` (`*string`).
- `DeadLetterSource` is **not** an application property. It comes from the message annotation `x-opt-deadletter-source`. On the emulator it was `nil` for explicit dead-letters, MaxDeliveryCount dead-letters and subscription DLQs (raw annotations held only `x-opt-enqueued-time`, `x-opt-locked-until`, `x-opt-message-state`, `x-opt-sequence-number`). My guess: Azure sets it only for auto-forwarded chains (not verified). Don't derive the Resubmit Target from it; use the entity the user opened.
- `DeadLetterOptions.PropertiesToModify` (`dlBy: spike`) shows up as an ordinary application property in the DLQ.

Outgoing message: build a new `azservicebus.Message`. Its fields are `ApplicationProperties, Body, ContentType, CorrelationID, MessageID, PartitionKey, ReplyTo, ReplyToSessionID, ScheduledEnqueueTime, SessionID, Subject, TimeToLive, To`. It has no dead-letter fields, so the only thing to strip is the two marker keys in `ApplicationProperties`. Broker-owned fields (sequence number, enqueued time, locked until, delivery count, state, dead-letter source) cannot be set on `Message` at all.

Application property types round-trip with their Go types: `string`, `int32`, `int64`, `float64`, `bool`, `time.Time` (arrives in `time.Local`, same instant). Copying the map as-is keeps the types.

`TimeToLive` came back as 30 m (as sent), `To`, `ReplyTo`, `CorrelationID`, `Subject`, `ContentType` were all preserved on the copy.

Emulator quirk: `CompleteMessage` on an already-completed message returned `nil` (twice). Don't build tests that expect an error there.

## 4. Duplicate detection

Confirmed on `orders-dedup` (`RequiresDuplicateDetection=true`, window PT5M):

- Two sends with the same `MessageID`: both `SendMessage` calls returned `nil`; the queue held **1** message (the first body).
- **Repair with the same MessageId:** dead-letter it, then send the copy with the original id and complete the original. The send returned `nil` and the complete succeeded. Result: active count **0**, DLQ count **0**. **The message is lost**, with no error anywhere. A copy with a new MessageId arrived normally.
- This confirms §6.1: use a new MessageId by default on duplicate-detecting targets.
- `admin.GetQueue` reports `RequiresDuplicateDetection` and `DuplicateDetectionHistoryTimeWindow` correctly on the emulator (`true`, `PT5M`).
- Emulator limit: `DuplicateDetectionHistoryTimeWindow` max **PT5M**. The emulator refuses to start with PT10M ("Max Duplicate detection window supported 5m").

## 5. DLQ of a sessionful queue with a plain receiver

- `NewReceiverForQueue("sessions-q", {SubQueue: SubQueueDeadLetter})`: **peek and peek-lock receive both work** (the DLQ is not sessionful). `SessionID` (`"s1"`) is preserved on the DLQ message.
- Repair back to `sessions-q` works when `SessionID` is copied. Without it the send fails with `amqp:not-allowed` ("The SessionId was not set on a message…").
- **Plain receiver on the sessionful queue's active side fails**, both `ReceiveMessages` and **`PeekMessages`**: `*amqp.Error` `amqp:not-allowed`, "It is not possible for an entity that requires sessions to create a non-sessionful message receiver." Peeking a sessionful queue needs a session receiver (`AcceptSessionForQueue`), which locks the session, so it is out for v0.1 (principle 1).

## 6. Admin client against the emulator

| Attempt | Result |
|---|---|
| `admin.NewClientFromConnectionString(<AMQP cs, :5682>)`, default transport | hangs until ctx deadline. The SDK always builds `https://<host:port>/` (`internal/atom/entity_manager.go`), and that port speaks AMQP, not HTTP. |
| admin cs `sb://localhost:5310`, default transport | hangs until ctx deadline (TLS against plain HTTP) |
| admin cs `sb://localhost:5310` + custom `azcore` transport that rewrites the scheme to `http` | **works** for entity calls |
| AMQP cs without port, http-only transport | hangs (goes to :80) |

- `UseDevelopmentEmulator=true` is accepted but does **not** switch the admin client to HTTP. The fix is the transport hack from Microsoft's own Go emulator sample (`Sample-Code-Snippets/Go/...`: `admin.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: httpOnlyTransport}}`). The emulator README says SDKs other than .NET "do not honor non-TLS connections or custom ports" for management.
- **Port:** taken from the Endpoint (`u.Host` keeps the port). A non-default host port (5310) works, so 5300 is not hardcoded. The AMQP client also honours the port: `amqp://localhost:5682/`.
- The emulator's admin endpoint does **not check auth**: plain `curl http://localhost:5310/orders?api-version=2021-05` returns the entity.

What works with the HTTP transport:

- `NewListQueuesPager`, `GetQueue` (properties: RequiresSession, RequiresDuplicateDetection, window, MaxDeliveryCount)
- `NewListTopicsPager`, `GetTopic`, `NewListSubscriptionsPager`, `GetSubscription`
- `NewListRulesPager` (`$Default` → `*admin.TrueFilter`, `shippable-only` → `*admin.SQLFilter{Expression: "eventType = 'OrderShipped'"}`)
- `GetNamespaceProperties` (`Name: sbemulatorns`)
- `CreateTopic`

What fails (runtime counts):

- `GetQueueRuntimeProperties`, `NewListQueuesRuntimePropertiesPager`, `GetTopicRuntimeProperties`: `*errors.errorString` "invalid queue/topic runtime properties: no CountDetails element". The emulator's entity XML has `MessageCount` but no `CountDetails`, so there is no DLQ count.
- `GetSubscriptionRuntimeProperties` and `NewListSubscriptionsRuntimePropertiesPager`: **panic** (nil pointer dereference in `admin.newSubscriptionRuntimePropertiesItem`, `admin_client_subscription.go:501`). This is an SDK bug; the call must be wrapped in `recover()` or never made against the emulator.

`Get*` for a missing entity returns `(nil, nil)`, not an error (`GetQueue("does-not-exist")`, `GetTopic("empty-topic")` before creating it). Callers must nil-check the response.

Fallback for `--emulator`: list entities through the admin client (HTTP transport) and show DLQ/active counts as `?`. Optionally count a DLQ by peeking pages until empty (cheap locally; never on Azure). Reading `emulator/config.json` is not needed, since listing works.

## 7. Error classification (for §6 step 4)

Most broker errors are **not** wrapped in `*azservicebus.Error`. Classify on `*amqp.Error` (`errors.As(err, &amqpErr)`, `amqpErr.Condition`) first, then on `*azservicebus.Error` `.Code`, then on context errors.

| Case | What `SendMessage` returned | Class |
|---|---|---|
| send to nonexistent queue | `*amqp.Error` `amqp:not-found` ("messaging entity … could not be found"), 0.3 s | definite |
| sessionful queue, no SessionID | `*amqp.Error` `amqp:not-allowed` | definite |
| sender on a subscription path (`topic/Subscriptions/sub`) | `*amqp.Error` `amqp:not-allowed` ("Cannot create a message sender for a subscription") | definite |
| 2 MiB body | `azservicebus.ErrMessageTooLarge` (client-side, before any send) | definite |
| namespace throttled | `*amqp.Error` `com.microsoft:server-busy` ("…namespace sbemulatorns is being throttled. Error code : 50002…") after the SDK's own retries | definite (broker refused) |
| ctx deadline already short (1 ms) | `context.DeadlineExceeded` (unwrapped) | ambiguous |
| sender on a DLQ path (`invoices/$DeadLetterQueue`) | hung until ctx deadline (30 s), `context.DeadlineExceeded` | ambiguous (bad target; the guard "repair only to queue/topic" prevents it) |
| nothing listening (wrong port) | `*azservicebus.Error` Code `connlost` (dial refused) | ambiguous per spec; this one is in fact pre-send. Optional refinement: treat a dial error (`*net.OpError`) as definite. |
| wrong SAS key | `nil`: **the emulator does not check keys**, so `unauthorized` can't be tested locally | — |
| send to topic with 0 subscriptions (`empty-topic`) | `nil`: **silently dropped**, confirms the §6.1 guard | — |
| peek nonexistent DLQ | `*amqp.Error` `amqp:not-found` | — |

`azservicebus.Code` values in v1.10.0: `unauthorized`, `connlost`, `locklost`, `timeout`, `notfound`, `closed`. The SDK only produces them for: lock lost; `com.microsoft:timeout` → `timeout`; closed; `amqp:unauthorized-access` → `unauthorized`; management RPC 401/404 → `unauthorized`/`notfound`; link/conn recovery → `connlost`. `amqp:not-found` on a send link stays a raw `*amqp.Error`.

Proposed classifier:

- **Definite** (abandon → SendFailed):
  - `*amqp.Error` with condition in {`amqp:not-found`, `amqp:not-allowed`, `amqp:unauthorized-access`, `amqp:link:message-size-exceeded`, `amqp:resource-limit-exceeded`, `com.microsoft:entity-disabled`, `com.microsoft:server-busy`}
  - `azservicebus.Error` Code `unauthorized` / `notfound`
  - `errors.Is(err, azservicebus.ErrMessageTooLarge)`
- **Ambiguous** (abandon → SendUncertain): everything else, including `context.DeadlineExceeded`/`Canceled`, Code `timeout`, `connlost`, `closed`, `*amqp.LinkError`/`*amqp.ConnError`, and unknown errors (safe default).

## Emulator notes (for tools/seed and E2E tests)

- **Config limits:** a topic with zero subscriptions is rejected at startup ("At least one subscription required per topic"), so `empty-topic` is **not** in `config.json`. It can be created at runtime with `admin.CreateTopic` (HTTP transport), but **runtime-created entities disappear on emulator restart**. `tools/seed` must (re)create `empty-topic` on every run. Duplicate-detection window max PT5M.
- **Throttling:** after the 1 465-abandon loop the emulator throttled the namespace (`com.microsoft:server-busy`, 50002) for about a minute. Single operations then stalled 5–10 s (sends, first receive on a new link, `ReceiveAndDelete` purge returning 0 in 2 s while messages remained). E2E tests need generous timeouts (≥ 30 s per op) and must not loop abandons.
- The admin endpoint's XML reports the default `DuplicateDetectionHistoryTimeWindow` (PT10M) in raw listings for queues that have duplicate detection off. Only trust it when `RequiresDuplicateDetection=true`.

## Spec adjustments (proposed; main session edits docs/spec.md)

1. **§6 step 3.1:** replace "Abandon every non-matching message immediately, await every abandon…" with:
   > Receive in peek-lock in small batches (`ReceiveMessages(ctx, b)`; the Go SDK has no prefetch option, so request exactly `b`). **Keep the locks on non-matching messages until the target is found or the scan limit K is reached, then abandon all of them at once**, await every abandon, log abandon errors (never swallow them) (BusX #196). Abandoning a message before the next receive puts it back at the head and the scan never advances (S−1 spike). The locks live only for the scan (milliseconds) and are released before the call returns.

   Also change §6 step 3.1 "**prefetch 0**" to "no prefetch (Go SDK: credits = batch size per `ReceiveMessages`; leftover credits are released by the SDK)".
2. **§6 step 2 / §12 Q2 / README scan-cost text:** "their DeliveryCount +1" stays as the warning, but add: "verified on active queues; on the emulator DLQ abandon did not change DeliveryCount; verify on a real namespace before S4 README". Optionally reword to "may increase DeliveryCount by 1".
3. **§6 step 3.2:** add "strip only the application-property keys `DeadLetterReason` and `DeadLetterErrorDescription`; `DeadLetterSource` is a broker annotation and cannot be set on an outgoing message. Copy `ApplicationProperties` values as-is to keep their types." Also note: "`SessionID` must be copied; a sessionful target rejects a message without one (`amqp:not-allowed`, definite)."
4. **§6 step 3.3:** keep it; `LockedUntil` is available on received messages. Add: "ignore `LockedUntil` on peeked messages".
5. **§6 step 3.4:** replace the definite/ambiguous examples with the classifier from item 7 above: definite = `*amqp.Error` conditions {not-found, not-allowed, unauthorized-access, link:message-size-exceeded, resource-limit-exceeded, com.microsoft:entity-disabled, com.microsoft:server-busy}, `azservicebus.Error` Code unauthorized/notfound, `ErrMessageTooLarge`; everything else ambiguous.
6. **§6 Target:** "Target: the entity the user opened (queue for a queue DLQ; the topic for a subscription DLQ), not `DeadLetterSource`, which is usually empty."
7. **§7 `--emulator`:** replace "admin endpoint on port 5300 — verify the Go admin client works against it in S1; if not, entity listing falls back to the emulator's config and counts show `?`" with:
   > AMQP from the connection string's Endpoint (default port 5672; `sb://host:port` is honoured). Admin client on `sb://<host>:<admin port>` (default 5300, flag `--emulator-admin-port`) with an HTTP-only `azcore` transport, because the SDK always uses https. Entity listing, properties and rules work. **Runtime counts do not**: the emulator omits `CountDetails`, queue/topic runtime calls error and subscription runtime calls panic in SDK v1.10.0. With `--emulator`, never call the runtime-properties APIs; counts show `?`. Wrap all admin calls in `recover()` anyway. `Get*` returns `(nil, nil)` for a missing entity.

   Decide the flag shape: `--emulator` default `localhost:5672` + `5300`; lazybus' own compose uses 5682/5310, so either `--emulator localhost:5682,5310` or `LAZYBUS_EMULATOR_AMQP`/`_ADMIN` env vars.
8. **§4 panel 2 (Entities):** "DLQ count `?` when runtime counts are unavailable (emulator)". **§10 S1a acceptance:** emulator E2E checks entity listing with `?` counts, not numbers.
9. **§4 panel 3 / §10 S1b (Active tab):** "Active tab on a sessionful entity shows 'sessionful: active messages need a session lock (not in v0.1)'". A plain receiver's peek fails with `amqp:not-allowed`. The DLQ tab works.
10. **§6.1 duplicate detection:** replace "Verify the behaviour in the S−1 spike" with "Verified (S−1): same-MessageId repair within the window ends with the copy dropped and the original completed, so the message is lost with no error." Detect the target's `RequiresDuplicateDetection` via `admin.GetQueue`/`GetTopic` (works on the emulator).
11. **§6.1 zero-subscription topic:** verified, the send to a topic with 0 subscriptions returns `nil` and the message is dropped. Subscription count must come from `NewListSubscriptionsPager` (works on the emulator), **not** `GetTopicRuntimeProperties().SubscriptionCount` (fails on the emulator).
12. **§10 S1a `tools/seed`:** must create `empty-topic` at runtime via admin (the emulator config cannot hold a topic without subscriptions, and runtime entities vanish on restart). E2E timeouts ≥ 30 s per operation. Don't loop abandons (throttling).
13. **§3 Stack:** pin `azservicebus v1.10.0`, `go-amqp v1.4.0` (needed directly for `*amqp.Error` classification).
14. **§9 / README:** document the local emulator: `cd emulator && cp .env.example .env && docker compose up -d` (AMQP 5682, admin 5310).

## Not covered

- `LockLost` path (abandon/complete after lock expiry): not run (1-minute lock duration; time-boxed). Expect `azservicebus.Error` Code `locklost`.
- Real-namespace checks: DLQ abandon DeliveryCount effect, `DeadLetterSource` population, `unauthorized` errors, runtime counts. None of these can be checked on the emulator.
