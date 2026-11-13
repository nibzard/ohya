(***************************************************************************)
(* ohya — sync cursor and acknowledgment protocol.                         *)
(*                                                                         *)
(* This module is the executable TLA+ model of the "Sync contract" in      *)
(* ARCHITECTURE.md: a device uploads contiguous batches of one source's    *)
(* records to the hub, the hub commits events and its authoritative        *)
(* cursor in one durable transaction, and the device persists the          *)
(* acknowledged cursor locally. It models batch upload, cursor             *)
(* advancement, retries, crashes, and lost acknowledgments.                *)
(*                                                                         *)
(* Each source models one device's uploader; several sources in one model  *)
(* synchronize concurrently on separate connections. Device identity,      *)
(* authentication, and the source-ownership binding are the authorization  *)
(* model (backlog T016), not this one.                                     *)
(*                                                                         *)
(* Scope and assumptions are recorded in tla/README.md. The companion      *)
(* configurations check this spec with TLC:                                *)
(*   SyncConcurrent.cfg — two sources, loss and crashes enabled            *)
(*   SyncFailures.cfg   — one source, deeper file, loss and crashes        *)
(*   SyncLiveness.cfg   — reliable network, fairness, liveness             *)
(***************************************************************************)

---- MODULE Sync ----

EXTENDS Naturals, FiniteSets

CONSTANTS
  MaxLen,      \* records one source file may reach (model bound)
  MaxBatch,    \* records per batch; batches may split a file
  Sources,     \* set of sources; each source is one watched file
  ReliableNet  \* BOOLEAN; FALSE enables message loss and crashes

VARIABLES
  \* Records appended to each source file. A source file is append-only:
  \* replacement and truncation are outside this model (ARCHITECTURE.md
  \* assigns them to new source incarnations).
  fileLen,
  \* The device's durably persisted cursor: the acknowledgment checkpoint.
  \* A device keeps no volatile protocol state beyond what is on the wire,
  \* so a crash loses only in-flight messages, never the checkpoint.
  ackCursor,
  \* The hub's durably committed cursor per source. It moves only inside
  \* the same transaction that stores events (sources.cursor_bytes).
  hubCursor,
  \* Record indices durably stored at the hub, per source.
  stored,
  \* Aggregate event count per source. It advances only for genuinely new
  \* inserts, so a replayed batch must leave it unchanged.
  eventCount,
  \* The one in-flight batch per source, traveling device -> hub.
  \* NoBatch means no batch is in flight.
  inflight,
  \* The acknowledgment traveling hub -> device, per source. Acks are
  \* monotone cursor values, so the wire keeps only the highest one.
  \* NoAck means no ack is in flight.
  wireAck

vars == <<fileLen, ackCursor, hubCursor, stored, eventCount, inflight, wireAck>>

(* No-message sentinels keep each channel single-typed, because TLC      *)
(* rejects equality across types and TLA+ has no negative numbers.       *)
(* NoBatch is the empty tuple. NoAck sits above every real cursor.       *)
NoBatch == <<>>
NoAck == MaxLen + 1

BatchVal == {<<>>} \cup {b \in (0..MaxLen) \times (0..MaxLen) : b[1] < b[2]}

AckVal == 0..(MaxLen + 1)

IsBatch(b) == b # NoBatch

Max2(a, b) == IF a > b THEN a ELSE b

(* The newer of two acknowledgments; NoAck never survives a real ack. *)
AckMax(a, b) == IF a = NoAck THEN b ELSE IF b = NoAck THEN a ELSE Max2(a, b)

(***********************************************************************)
(* Initial state. Every source starts registered and empty: the source  *)
(* registration step is not modeled (T017 owns it). All cursors are 0,  *)
(* nothing is stored, and no messages are in flight.                    *)
(***********************************************************************)

Init ==
  /\ fileLen    = [s \in Sources |-> 0]
  /\ ackCursor  = [s \in Sources |-> 0]
  /\ hubCursor  = [s \in Sources |-> 0]
  /\ stored     = [s \in Sources |-> {}]
  /\ eventCount = [s \in Sources |-> 0]
  /\ inflight   = [s \in Sources |-> NoBatch]
  /\ wireAck    = [s \in Sources |-> NoAck]

(***********************************************************************)
(* Actions                                                              *)
(***********************************************************************)

(* The coding agent appends one record to a source file. The append may *)
(* interleave with synchronization arbitrarily. Not fair: the protocol  *)
(* must tolerate any append schedule, and liveness only promises that   *)
(* appended records eventually sync.                                    *)
AppendRecord(s) ==
  /\ fileLen[s] < MaxLen
  /\ fileLen' = [fileLen EXCEPT ![s] = fileLen[s] + 1]
  /\ UNCHANGED <<ackCursor, hubCursor, stored, eventCount, inflight, wireAck>>

(* The device builds the next batch from its durable checkpoint: bytes  *)
(* from the acknowledged cursor up to the batch bound. It sends only    *)
(* when no batch is in flight (one in-flight batch per source) and      *)
(* never infers success from having sent.                               *)
SendBatch(s) ==
  /\ inflight[s] = NoBatch
  /\ ackCursor[s] < fileLen[s]
  /\ inflight' = [inflight EXCEPT ![s] =
       <<ackCursor[s],
         IF fileLen[s] - ackCursor[s] =< MaxBatch
           THEN fileLen[s]
           ELSE ackCursor[s] + MaxBatch>>]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, wireAck>>

(* The hub's durable transaction, atomic by construction: insert only   *)
(* the records not yet stored (those at or beyond the committed cursor) *)
(* count them once, advance the committed cursor over contiguous bytes, *)
(* and emit the acknowledgment. A fully repeated batch inserts nothing, *)
(* counts nothing, and returns the existing cursor.                     *)
(*                                                                      *)
(* The guard below restates the contract rule "reject gaps ahead of the *)
(* stored cursor": a batch must start at or behind the committed        *)
(* cursor. Batches are built from ackCursor, which never passes         *)
(* hubCursor, so the guard always holds here; the hub checks it anyway. *)
HubCommit(s) ==
  /\ IsBatch(inflight[s])
  /\ LET lo    == inflight[s][1]
         hi    == inflight[s][2]
         fresh == {i \in hubCursor[s] .. (hi - 1) : TRUE}
     IN /\ lo =< hubCursor[s]
        /\ stored'     = [stored EXCEPT ![s] = stored[s] \cup fresh]
        /\ eventCount' = [eventCount EXCEPT ![s] = eventCount[s] + Cardinality(fresh)]
        /\ hubCursor'  = [hubCursor EXCEPT ![s] = Max2(hubCursor[s], hi)]
        /\ inflight'   = [inflight EXCEPT ![s] = NoBatch]
        /\ wireAck'    = [wireAck EXCEPT ![s] = AckMax(wireAck[s], hi)]
        /\ UNCHANGED <<fileLen, ackCursor>>

(* The device persists the acknowledged cursor durably and atomically.  *)
(* Acks are monotone, so a stale ack arriving late changes nothing.     *)
DeliverAck(s) ==
  /\ wireAck[s] # NoAck
  /\ ackCursor' = [ackCursor EXCEPT ![s] = Max2(ackCursor[s], wireAck[s])]
  /\ wireAck'   = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, hubCursor, stored, eventCount, inflight>>

(* A batch is lost, or its connection times out and is reset. Either    *)
(* way the device learns nothing and retries from ackCursor.            *)
DropBatch(s) ==
  /\ ReliableNet = FALSE
  /\ IsBatch(inflight[s])
  /\ inflight' = [inflight EXCEPT ![s] = NoBatch]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, wireAck>>

(* An acknowledgment is lost in transit. The device keeps its stale     *)
(* checkpoint and retries; the hub replays the overlap idempotently.    *)
DropAck(s) ==
  /\ ReliableNet = FALSE
  /\ wireAck[s] # NoAck
  /\ wireAck' = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, inflight>>

(* The device process owning s crashes. Its durable checkpoint          *)
(* survives; its connection resets, so the batch and the ack in flight  *)
(* for that source vanish together. This is the crash window between    *)
(* hub commit and local persistence. A device owning several sources    *)
(* crashes them together; the model may crash each source independently,*)
(* which explores a superset of those grouped behaviors.                *)
CrashClient(s) ==
  /\ ReliableNet = FALSE
  /\ IsBatch(inflight[s]) \/ wireAck[s] # NoAck
  /\ inflight' = [inflight EXCEPT ![s] = NoBatch]
  /\ wireAck'  = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount>>

(* The hub process crashes. Committed state survives (WAL, synchronous  *)
(* FULL); uncommitted work vanishes by atomicity. Every connection      *)
(* resets, so all in-flight batches and acks are lost. No cursor ever   *)
(* rolls back: intact hub storage is an assumption.                     *)
CrashHub ==
  /\ ReliableNet = FALSE
  /\ \E s \in Sources : IsBatch(inflight[s]) \/ wireAck[s] # NoAck
  /\ inflight' = [t \in Sources |-> NoBatch]
  /\ wireAck'  = [t \in Sources |-> NoAck]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount>>

Next ==
  \/ \E s \in Sources :
       \/ AppendRecord(s)
       \/ SendBatch(s)
       \/ HubCommit(s)
       \/ DeliverAck(s)
       \/ DropBatch(s)
       \/ DropAck(s)
       \/ CrashClient(s)
  \/ CrashHub

(***********************************************************************)
(* Safety invariants.                                                  *)
(* Checked by every configuration below.                               *)
(***********************************************************************)

TypeOK ==
  /\ fileLen    \in [Sources -> 0..MaxLen]
  /\ ackCursor  \in [Sources -> 0..MaxLen]
  /\ hubCursor  \in [Sources -> 0..MaxLen]
  /\ stored     \in [Sources -> SUBSET 0..(MaxLen - 1)]
  /\ eventCount \in [Sources -> 0..MaxLen]
  /\ inflight   \in [Sources -> BatchVal]
  /\ wireAck    \in [Sources -> AckVal]

(* Cursor advancement never precedes durable storage: no acknowledged   *)
(* cursor passes the hub's committed cursor.                            *)
AckLeCommit == \A s \in Sources : ackCursor[s] =< hubCursor[s]

(* Ordering: the hub stores exactly the contiguous prefix below its     *)
(* committed cursor. Records of one source become visible in file       *)
(* order, whatever the interleaving across sources. No phantom record   *)
(* sits beyond the cursor.                                              *)
StoredPrefixContiguous ==
  \A s \in Sources : stored[s] = (0..(hubCursor[s] - 1))

(* Retries and reconnections create no duplicate events: aggregates     *)
(* count each stored record exactly once. A replayed batch adds none.   *)
EventsCountedOnce ==
  \A s \in Sources : eventCount[s] = Cardinality(stored[s])

(* Events are not lost after acknowledged commits: every record below   *)
(* an acknowledged cursor is durably stored at the hub.                 *)
AckedRecordsStored ==
  \A s \in Sources : \A i \in 0..(ackCursor[s] - 1) : i \in stored[s]

(* The hub never commits records the file does not hold.                *)
HubWithinFile == \A s \in Sources : hubCursor[s] =< fileLen[s]

(* A batch in flight never starts ahead of the durable checkpoint: the   *)
(* device never infers success from having sent a batch. It may start at *)
(* an older checkpoint than the current one, because an earlier ack can  *)
(* arrive while the next batch is already on the wire.                   *)
InflightFromAck ==
  \A s \in Sources : IsBatch(inflight[s]) => inflight[s][1] =< ackCursor[s]

(* An acknowledgment on the wire never passes the committed cursor.     *)
WireAckLeCommit ==
  \A s \in Sources : wireAck[s] # NoAck => wireAck[s] =< hubCursor[s]

(***********************************************************************)
(* Liveness.                                                            *)
(*                                                                      *)
(* Fairness: every device retries, the hub commits what it receives,    *)
(* and the device persists the ack it receives (weak fairness per       *)
(* source). The reliable-network model (ReliableNet = TRUE) states the  *)
(* assumption behind liveness: loss and crashes stop, and the device    *)
(* eventually reconnects and retries. Appends themselves stay unfair —  *)
(* the protocol must sync whatever has been appended, on any schedule.  *)
(***********************************************************************)

EventualSync ==
  /\ \A s \in Sources, i \in 0..(MaxLen - 1) :
       (fileLen[s] > i) ~> (i \in stored[s])
  /\ \A s \in Sources, i \in 0..(MaxLen - 1) :
       (i \in stored[s]) ~> (ackCursor[s] > i)

Fairness ==
  /\ \A s \in Sources : WF_vars(SendBatch(s))
  /\ \A s \in Sources : WF_vars(HubCommit(s))
  /\ \A s \in Sources : WF_vars(DeliverAck(s))

Spec == Init /\ [][Next]_vars /\ Fairness

(***********************************************************************)
(* Helper for humans reading traces. Not checked.                       *)
(***********************************************************************)

(* The quiescent state of a drained system: nothing is in flight and    *)
(* every cursor sits at the file end. It is a legitimate fixed point,   *)
(* so TLC runs with -deadlock; a reported deadlock there is expected.   *)
Quiescent ==
  \A s \in Sources :
    /\ inflight[s] = NoBatch
    /\ wireAck[s] = NoAck
    /\ ackCursor[s] = fileLen[s]
    /\ hubCursor[s] = fileLen[s]

====
