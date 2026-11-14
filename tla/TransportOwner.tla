---- MODULE TransportOwner ----

EXTENDS Naturals, FiniteSets

CONSTANT Processes

VARIABLES owner, phase, stacks, endpoint, borrowers

vars == <<owner, phase, stacks, endpoint, borrowers>>
None == "none"
Phases == {"idle", "locked", "running", "published", "closing", "stopped"}

Init ==
  /\ owner = None
  /\ phase = [p \in Processes |-> "idle"]
  /\ stacks = {}
  /\ endpoint = None
  /\ borrowers = {}

Acquire(p) ==
  /\ owner = None
  /\ phase[p] = "idle"
  /\ owner' = p
  /\ phase' = [phase EXCEPT ![p] = "locked"]
  /\ endpoint' = None
  /\ UNCHANGED <<stacks, borrowers>>

Start(p) ==
  /\ owner = p
  /\ phase[p] = "locked"
  /\ phase' = [phase EXCEPT ![p] = "running"]
  /\ stacks' = stacks \cup {p}
  /\ UNCHANGED <<owner, endpoint, borrowers>>

Publish(p) ==
  /\ owner = p
  /\ phase[p] = "running"
  /\ phase' = [phase EXCEPT ![p] = "published"]
  /\ endpoint' = p
  /\ UNCHANGED <<owner, stacks, borrowers>>

Borrow(p) ==
  /\ owner # None
  /\ p # owner
  /\ phase[p] = "idle"
  /\ endpoint = owner
  /\ phase[owner] = "published"
  /\ borrowers' = borrowers \cup {p}
  /\ UNCHANGED <<owner, phase, stacks, endpoint>>

Disconnect(p) ==
  /\ p \in borrowers
  /\ borrowers' = borrowers \ {p}
  /\ UNCHANGED <<owner, phase, stacks, endpoint>>

Withdraw(p) ==
  /\ owner = p
  /\ phase[p] \in {"locked", "running", "published"}
  /\ phase' = [phase EXCEPT ![p] = "closing"]
  /\ endpoint' = None
  /\ UNCHANGED <<owner, stacks, borrowers>>

Stop(p) ==
  /\ owner = p
  /\ phase[p] = "closing"
  /\ phase' = [phase EXCEPT ![p] = "stopped"]
  /\ stacks' = stacks \ {p}
  /\ borrowers' = {}
  /\ UNCHANGED <<owner, endpoint>>

Release(p) ==
  /\ owner = p
  /\ phase[p] = "stopped"
  /\ owner' = None
  /\ phase' = [phase EXCEPT ![p] = "idle"]
  /\ UNCHANGED <<stacks, endpoint, borrowers>>

Crash(p) ==
  /\ owner = p
  /\ owner' = None
  /\ phase' = [phase EXCEPT ![p] = "idle"]
  /\ stacks' = stacks \ {p}
  /\ borrowers' = {}
  /\ UNCHANGED endpoint

Next == \E p \in Processes:
  Acquire(p) \/ Start(p) \/ Publish(p) \/ Borrow(p) \/ Disconnect(p) \/
  Withdraw(p) \/ Stop(p) \/ Release(p) \/ Crash(p)

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ owner \in Processes \cup {None}
  /\ phase \in [Processes -> Phases]
  /\ stacks \subseteq Processes
  /\ endpoint \in Processes \cup {None}
  /\ borrowers \subseteq Processes

OneStack == Cardinality(stacks) <= 1
StackHasOwner == \A p \in stacks: owner = p
LockMatchesPhase ==
  \A p \in Processes: (phase[p] # "idle") <=> (owner = p)
BorrowersHaveStack == borrowers # {} => owner \in stacks
OwnerDoesNotBorrow == owner \notin borrowers

====
