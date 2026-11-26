---- MODULE Sync ----

EXTENDS Naturals, FiniteSets

CONSTANTS
  MaxLen,
  MaxBatch,
  Sources,
  ReliableNet,
  MaxPurges

VARIABLES
  fileLen,
  ackCursor,
  hubCursor,
  stored,
  eventCount,
  inflight,
  wireAck,
  purged,
  purgeMark,
  purgeCount,
  reallowCount

vars == <<fileLen, ackCursor, hubCursor, stored, eventCount, inflight, wireAck,
          purged, purgeMark, purgeCount, reallowCount>>

NoBatch == <<>>
NoAck == MaxLen + 1

BatchVal == {<<>>} \cup {b \in (0..MaxLen) \times (0..MaxLen) : b[1] < b[2]}

AckVal == 0..(MaxLen + 1)

IsBatch(b) == b # NoBatch

Max2(a, b) == IF a > b THEN a ELSE b

AckMax(a, b) == IF a = NoAck THEN b ELSE IF b = NoAck THEN a ELSE Max2(a, b)

Init ==
  /\ fileLen     = [s \in Sources |-> 0]
  /\ ackCursor   = [s \in Sources |-> 0]
  /\ hubCursor   = [s \in Sources |-> 0]
  /\ stored      = [s \in Sources |-> {}]
  /\ eventCount  = [s \in Sources |-> 0]
  /\ inflight    = [s \in Sources |-> NoBatch]
  /\ wireAck     = [s \in Sources |-> NoAck]
  /\ purged      = [s \in Sources |-> FALSE]
  /\ purgeMark   = [s \in Sources |-> 0]
  /\ purgeCount  = [s \in Sources |-> 0]
  /\ reallowCount = [s \in Sources |-> 0]

AppendRecord(s) ==
  /\ fileLen[s] < MaxLen
  /\ fileLen' = [fileLen EXCEPT ![s] = fileLen[s] + 1]
  /\ UNCHANGED <<ackCursor, hubCursor, stored, eventCount, inflight, wireAck,
                 purged, purgeMark, purgeCount, reallowCount>>

SendBatch(s) ==
  /\ inflight[s] = NoBatch
  /\ ackCursor[s] < fileLen[s]
  /\ inflight' = [inflight EXCEPT ![s] =
       <<ackCursor[s],
         IF fileLen[s] - ackCursor[s] =< MaxBatch
           THEN fileLen[s]
           ELSE ackCursor[s] + MaxBatch>>]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, wireAck,
                 purged, purgeMark, purgeCount, reallowCount>>

HubCommit(s) ==
  /\ ~purged[s]
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
        /\ UNCHANGED <<fileLen, ackCursor, purged, purgeMark, purgeCount, reallowCount>>

DeliverAck(s) ==
  /\ wireAck[s] # NoAck
  /\ ackCursor' = [ackCursor EXCEPT ![s] = Max2(ackCursor[s], wireAck[s])]
  /\ wireAck'   = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, hubCursor, stored, eventCount, inflight,
                 purged, purgeMark, purgeCount, reallowCount>>

PurgeSource(s) ==
  /\ purgeCount[s] < MaxPurges
  /\ ~purged[s]
  /\ hubCursor[s] > 0
  /\ purged'       = [purged EXCEPT ![s] = TRUE]
  /\ purgeMark'    = [purgeMark EXCEPT ![s] = hubCursor[s]]
  /\ purgeCount'   = [purgeCount EXCEPT ![s] = purgeCount[s] + 1]
  /\ stored'       = [stored EXCEPT ![s] = {}]
  /\ eventCount'   = [eventCount EXCEPT ![s] = 0]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, inflight, wireAck, reallowCount>>

ReallowSource(s) ==
  /\ purged[s]
  /\ reallowCount[s] < purgeCount[s]
  /\ purged'       = [purged EXCEPT ![s] = FALSE]
  /\ reallowCount' = [reallowCount EXCEPT ![s] = reallowCount[s] + 1]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, inflight,
                 wireAck, purgeMark, purgeCount>>

DropBatch(s) ==
  /\ ReliableNet = FALSE
  /\ IsBatch(inflight[s])
  /\ inflight' = [inflight EXCEPT ![s] = NoBatch]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, wireAck,
                 purged, purgeMark, purgeCount, reallowCount>>

DropAck(s) ==
  /\ ReliableNet = FALSE
  /\ wireAck[s] # NoAck
  /\ wireAck'   = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount, inflight,
                 purged, purgeMark, purgeCount, reallowCount>>

CrashClient(s) ==
  /\ ReliableNet = FALSE
  /\ IsBatch(inflight[s]) \/ wireAck[s] # NoAck
  /\ inflight' = [inflight EXCEPT ![s] = NoBatch]
  /\ wireAck'  = [wireAck EXCEPT ![s] = NoAck]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount,
                 purged, purgeMark, purgeCount, reallowCount>>

CrashHub ==
  /\ ReliableNet = FALSE
  /\ \E s \in Sources : IsBatch(inflight[s]) \/ wireAck[s] # NoAck
  /\ inflight' = [t \in Sources |-> NoBatch]
  /\ wireAck'  = [t \in Sources |-> NoAck]
  /\ UNCHANGED <<fileLen, ackCursor, hubCursor, stored, eventCount,
                 purged, purgeMark, purgeCount, reallowCount>>

Next ==
  \/ \E s \in Sources :
       \/ AppendRecord(s)
       \/ SendBatch(s)
       \/ HubCommit(s)
       \/ DeliverAck(s)
       \/ PurgeSource(s)
       \/ ReallowSource(s)
       \/ DropBatch(s)
       \/ DropAck(s)
       \/ CrashClient(s)
  \/ CrashHub

TypeOK ==
  /\ fileLen      \in [Sources -> 0..MaxLen]
  /\ ackCursor    \in [Sources -> 0..MaxLen]
  /\ hubCursor    \in [Sources -> 0..MaxLen]
  /\ stored       \in [Sources -> SUBSET 0..(MaxLen - 1)]
  /\ eventCount   \in [Sources -> 0..MaxLen]
  /\ inflight     \in [Sources -> BatchVal]
  /\ wireAck      \in [Sources -> AckVal]
  /\ purged       \in [Sources -> BOOLEAN]
  /\ purgeMark    \in [Sources -> 0..MaxLen]
  /\ purgeCount   \in [Sources -> 0..MaxPurges]
  /\ reallowCount \in [Sources -> 0..MaxPurges]

AckLeCommit == \A s \in Sources : ackCursor[s] =< hubCursor[s]

StoredPrefixContiguous ==
  \A s \in Sources : stored[s] = purgeMark[s]..(hubCursor[s] - 1)

NoResurrection ==
  \A s \in Sources : \A i \in 0..(purgeMark[s] - 1) : i \notin stored[s]

PurgedSourceStoresNothing ==
  \A s \in Sources : purged[s] => stored[s] = {}

TombstoneConsistent ==
  \A s \in Sources : purged[s] = (purgeCount[s] > reallowCount[s])

EventsCountedOnce ==
  \A s \in Sources : eventCount[s] = Cardinality(stored[s])

AckedRecordsStored ==
  \A s \in Sources :
    \A i \in 0..(ackCursor[s] - 1) : i \in stored[s] \/ i < purgeMark[s]

HubWithinFile == \A s \in Sources : hubCursor[s] =< fileLen[s]

InflightFromAck ==
  \A s \in Sources : IsBatch(inflight[s]) => inflight[s][1] =< ackCursor[s]

WireAckLeCommit ==
  \A s \in Sources : wireAck[s] # NoAck => wireAck[s] =< hubCursor[s]

HubCursorNeverRegresses ==
  [][\A s \in Sources : hubCursor'[s] >= hubCursor[s]]_vars

PurgeMarkNeverRegresses ==
  [][\A s \in Sources : purgeMark'[s] >= purgeMark[s]]_vars

EventualSync ==
  /\ \A s \in Sources, i \in 0..(MaxLen - 1) :
       (~purged[s] /\ fileLen[s] > i) ~> (i \in stored[s] \/ i < purgeMark[s] \/ purged[s])
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
