---- MODULE Sync ----

EXTENDS Naturals, FiniteSets

CONSTANTS
  MaxLen,
  MaxBatch,
  Sources,
  ReliableNet

VARIABLES
  fileLen,
  ackCursor,
  hubCursor,
  stored,
  eventCount,
  inflight,
  wireAck

vars == <<fileLen, ackCursor, hubCursor, stored, eventCount, inflight, wireAck>>

NoBatch == <<>>
NoAck == MaxLen + 1

BatchVal == {<<>>} \cup {b \in (0..MaxLen) \times (0..MaxLen) : b[1] < b[2]}

AckVal == 0..(MaxLen + 1)

IsBatch(b) == b # NoBatch

Max2(a, b) == IF a > b THEN a ELSE b

AckMax(a, b) == IF a = NoAck THEN b ELSE IF b = NoAck THEN a ELSE Max2(a, b)

Init ==
  /\ fileLen    = [s \in Sources |-> 0]
  /\ ackCursor  = [s \in Sources |-> 0]
  /\ hubCursor  = [s \in Sources |-> 0]
  /\ stored     = [s \in Sources |-> {}]
  /\ eventCount = [s \in Sources |-> 0]
  /\ inflight   = [s \in Sources |-> NoBatch]
  /\ wireAck    = [s \in Sources |-> NoAck]

AppendRecord(s) ==
  /\ fileLen[s] < MaxLen
  /\ fileLen' = [fileLen EXCEPT ![s] = fileLen[s] + 1]
  /\ UNCHANGED <<ackCursor, hubCursor, stored, eventCount, inflight, wireAck>>

SendBatch(s) ==
  /\ inflight[s] = NoBatch
  /\ ackCursor[s] < fileLen[s]
  /\ inflight' = [inflight EXCEPT ![s] =
       <<ackCursor[s],
         IF fileLen[s] - ackCursor[s] =< MaxBatch
           THEN fileLen[s]
           ELSE ackCursor[s] + MaxBatch>>]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, wireAck>>

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

DeliverAck(s) ==
  /\ wireAck[s] # NoAck
  /\ ackCursor' = [ackCursor EXCEPT ![s] = Max2(ackCursor[s], wireAck[s])]
  /\ wireAck'   = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, hubCursor, stored, eventCount, inflight>>

DropBatch(s) ==
  /\ ReliableNet = FALSE
  /\ IsBatch(inflight[s])
  /\ inflight' = [inflight EXCEPT ![s] = NoBatch]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, wireAck>>

DropAck(s) ==
  /\ ReliableNet = FALSE
  /\ wireAck[s] # NoAck
  /\ wireAck' = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, inflight>>

CrashClient(s) ==
  /\ ReliableNet = FALSE
  /\ IsBatch(inflight[s]) \/ wireAck[s] # NoAck
  /\ inflight' = [inflight EXCEPT ![s] = NoBatch]
  /\ wireAck'  = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount>>

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

TypeOK ==
  /\ fileLen    \in [Sources -> 0..MaxLen]
  /\ ackCursor  \in [Sources -> 0..MaxLen]
  /\ hubCursor  \in [Sources -> 0..MaxLen]
  /\ stored     \in [Sources -> SUBSET 0..(MaxLen - 1)]
  /\ eventCount \in [Sources -> 0..MaxLen]
  /\ inflight   \in [Sources -> BatchVal]
  /\ wireAck    \in [Sources -> AckVal]

AckLeCommit == \A s \in Sources : ackCursor[s] =< hubCursor[s]

StoredPrefixContiguous ==
  \A s \in Sources : stored[s] = (0..(hubCursor[s] - 1))

EventsCountedOnce ==
  \A s \in Sources : eventCount[s] = Cardinality(stored[s])

AckedRecordsStored ==
  \A s \in Sources : \A i \in 0..(ackCursor[s] - 1) : i \in stored[s]

HubWithinFile == \A s \in Sources : hubCursor[s] =< fileLen[s]

InflightFromAck ==
  \A s \in Sources : IsBatch(inflight[s]) => inflight[s][1] =< ackCursor[s]

WireAckLeCommit ==
  \A s \in Sources : wireAck[s] # NoAck => wireAck[s] =< hubCursor[s]

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

Quiescent ==
  \A s \in Sources :
    /\ inflight[s] = NoBatch
    /\ wireAck[s] = NoAck
    /\ ackCursor[s] = fileLen[s]
    /\ hubCursor[s] = fileLen[s]

====
