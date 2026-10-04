# lazybus — glossary

**Peek**:
Reading messages from an entity or its dead-letter queue without locking them and without changing their delivery count.
_Avoid_: browse-lock, receive (receive implies a lock or removal)

**Dead-letter Markers**:
The application properties `DeadLetterReason` and `DeadLetterErrorDescription` that Service Bus attaches when it dead-letters a message. A repaired message never carries them.

**Property Edit**:
A single typed change to one application property of a message: set a key (add or change) to a value of a named property type, or remove the key. Dead-letter Markers are never a Property Edit target; the repair drops them itself. A changed key keeps its existing type unless the edit names another.
_Avoid_: property override, header edit

**Pending Edits**:
The Property Edits, body edit and Subject/ContentType changes a user has prepared for one dead-letter message and not yet resubmitted. They live in memory only.

**Resubmit Target**:
The sendable entity — a queue or topic on the same namespace — that receives the repaired copy. Defaults to the dead-letter message's source entity.
_Avoid_: destination, forward-to (collides with the ForwardTo entity property)

**DLQ Repair**:
Taking one dead-letter message, optionally applying its Pending Edits, sending the result to the Resubmit Target and removing the original from the dead-letter queue, so no duplicate remains and the copy carries no Dead-letter Markers. A repair with zero edits is still a DLQ Repair. Sending a copy while the original stays dead-lettered is not.
_Avoid_: resend, replay, requeue (other tools use these for the copy-and-leave flow)
"Resubmit" is the user-facing verb for a DLQ Repair (the `r` key, the Resubmitted outcome).

**Finish Cleanup**:
Removing the original of a Resubmit Cleanup Pending message from the dead-letter queue without sending anything, so only the copy in the Resubmit Target remains.
_Avoid_: delete, purge (v0.1 has no general delete)

**Resubmit Cleanup Pending**:
The outcome of a DLQ Repair where the copy reached the Resubmit Target but the original could not be removed from the dead-letter queue — a copy now exists in both places. Reported distinctly, never as success or failure.
_Avoid_: partial resubmit, warning

**Send Uncertain**:
The outcome of a DLQ Repair where the send to the Resubmit Target ended ambiguously (timeout, dropped link): the original is back in the dead-letter queue and the copy may or may not exist in the target. Never retried automatically.

**Context Stack**:
The ordered set of UI contexts (side panels, main pane, popups, menus, filter) where only the top context receives keys and `esc` pops one level.

## Relationships

- A DLQ Repair holds a lock only for its own duration; nothing else in lazybus ever holds a lock.
- Pending Edits belong to exactly one dead-letter message; a DLQ Repair consumes them.
- A Resubmit Cleanup Pending message is resolved by a Finish Cleanup, never by a second DLQ Repair.
