---- MODULE Auth ----

EXTENDS Naturals, FiniteSets

CONSTANTS
  Devices,
  Invitations,
  Sessions,
  Sources,
  Annotations

VARIABLES
  invState,
  invUsedBy,
  registry,
  pairedVia,
  revokedEver,
  sesDevice,
  sesLive,
  srcOwner,
  srcEverOwned,
  annRows,
  annRowSes

vars == <<invState, invUsedBy, registry, pairedVia, revokedEver,
          sesDevice, sesLive, srcOwner, srcEverOwned, annRows, annRowSes>>

None == "none"

InvStateVal == {"unissued", "open", "used", "expired"}
RegStateVal  == {"absent", "active", "revoked"}

Authorized(c) ==
  /\ sesLive[c]
  /\ registry[sesDevice[c]] = "active"

Init ==
  /\ invState     = [i \in Invitations |-> "unissued"]
  /\ invUsedBy    = [i \in Invitations |-> None]
  /\ registry     = [d \in Devices |-> "absent"]
  /\ pairedVia    = [d \in Devices |-> None]
  /\ revokedEver  = {}
  /\ sesDevice    = [c \in Sessions |-> None]
  /\ sesLive      = [c \in Sessions |-> FALSE]
  /\ srcOwner     = [s \in Sources |-> None]
  /\ srcEverOwned = [s \in Sources |-> {}]
  /\ annRows      = [a \in Annotations |-> {}]
  /\ annRowSes    = [a \in Annotations |-> [d \in Devices |-> None]]

IssueInvitation(i) ==
  /\ invState[i] = "unissued"
  /\ invState'  = [invState EXCEPT ![i] = "open"]
  /\ UNCHANGED <<invUsedBy, registry, pairedVia, revokedEver, sesDevice,
                 sesLive, srcOwner, srcEverOwned, annRows, annRowSes>>

ExpireInvitation(i) ==
  /\ invState[i] = "open"
  /\ invState'  = [invState EXCEPT ![i] = "expired"]
  /\ UNCHANGED <<invUsedBy, registry, pairedVia, revokedEver, sesDevice,
                 sesLive, srcOwner, srcEverOwned, annRows, annRowSes>>

ConsumeInvitation(d, i) ==
  /\ invState[i] = "open"
  /\ registry[d] = "absent"
  /\ invState'  = [invState EXCEPT ![i] = "used"]
  /\ invUsedBy' = [invUsedBy EXCEPT ![i] = d]
  /\ registry'  = [registry EXCEPT ![d] = "active"]
  /\ pairedVia' = [pairedVia EXCEPT ![d] = i]
  /\ UNCHANGED <<revokedEver, sesDevice, sesLive, srcOwner, srcEverOwned,
                 annRows, annRowSes>>

RevokeDevice(d) ==
  /\ registry[d] = "active"
  /\ registry'   = [registry EXCEPT ![d] = "revoked"]
  /\ revokedEver' = revokedEver \cup {d}
  /\ sesLive'    = [c \in Sessions |->
                      IF sesLive[c] /\ sesDevice[c] = d
                        THEN FALSE
                        ELSE sesLive[c]]
  /\ UNCHANGED <<invState, invUsedBy, pairedVia, sesDevice, srcOwner,
                 srcEverOwned, annRows, annRowSes>>

OpenSession(d, c) ==
  /\ registry[d] = "active"
  /\ sesDevice[c] = None
  /\ sesDevice'  = [sesDevice EXCEPT ![c] = d]
  /\ sesLive'    = [sesLive EXCEPT ![c] = TRUE]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 srcOwner, srcEverOwned, annRows, annRowSes>>

CloseSession(c) ==
  /\ sesLive[c]
  /\ sesLive' = [sesLive EXCEPT ![c] = FALSE]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, srcOwner, srcEverOwned, annRows, annRowSes>>

Upload(c, s) ==
  /\ Authorized(c)
  /\ LET d == sesDevice[c]
     IN /\ srcOwner[s] \in {None, d}
        /\ srcOwner'     = [srcOwner EXCEPT ![s] = d]
        /\ srcEverOwned' = [srcEverOwned EXCEPT ![s] = srcEverOwned[s] \cup {d}]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, annRows, annRowSes>>

Annotate(c, a) ==
  /\ Authorized(c)
  /\ LET d == sesDevice[c]
     IN /\ annRows'   = [annRows EXCEPT ![a] = annRows[a] \cup {d}]
        /\ annRowSes' = [annRowSes EXCEPT ![a] =
                           [annRowSes[a] EXCEPT ![d] = c]]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, srcOwner, srcEverOwned>>

DeleteAnnotation(c, a) ==
  /\ Authorized(c)
  /\ LET d == sesDevice[c]
     IN /\ annRows'   = [annRows EXCEPT ![a] = annRows[a] \ {d}]
        /\ annRowSes' = [annRowSes EXCEPT ![a] =
                           [annRowSes[a] EXCEPT ![d] = c]]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, srcOwner, srcEverOwned>>

Next ==
  \/ \E i \in Invitations :
       \/ IssueInvitation(i)
       \/ ExpireInvitation(i)
  \/ \E d \in Devices :
       RevokeDevice(d)
  \/ \E d \in Devices, i \in Invitations :
       ConsumeInvitation(d, i)
  \/ \E d \in Devices, c \in Sessions :
       OpenSession(d, c)
  \/ \E c \in Sessions :
       CloseSession(c)
  \/ \E c \in Sessions, s \in Sources :
       Upload(c, s)
  \/ \E c \in Sessions, a \in Annotations :
       \/ Annotate(c, a)
       \/ DeleteAnnotation(c, a)

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ invState     \in [Invitations -> InvStateVal]
  /\ invUsedBy    \in [Invitations -> Devices \cup {None}]
  /\ registry     \in [Devices -> RegStateVal]
  /\ pairedVia    \in [Devices -> Invitations \cup {None}]
  /\ revokedEver  \in SUBSET Devices
  /\ sesDevice    \in [Sessions -> Devices \cup {None}]
  /\ sesLive      \in [Sessions -> BOOLEAN]
  /\ srcOwner     \in [Sources -> Devices \cup {None}]
  /\ srcEverOwned \in [Sources -> SUBSET Devices]
  /\ annRows      \in [Annotations -> SUBSET Devices]
  /\ annRowSes    \in [Annotations -> [Devices -> Sessions \cup {None}]]

SessionLiveHasDevice ==
  \A c \in Sessions : sesLive[c] => sesDevice[c] # None

InvitationSingleUse ==
  \A i \in Invitations :
    Cardinality({d \in Devices : pairedVia[d] = i}) =< 1

PairingConsistent ==
  \A d \in Devices :
    (pairedVia[d] # None) = (registry[d] # "absent")

InvitationUsedByPairer ==
  \A d \in Devices :
    pairedVia[d] # None =>
      /\ invState[pairedVia[d]] = "used"
      /\ invUsedBy[pairedVia[d]] = d

NoLiveSessionForRevoked ==
  \A c \in Sessions :
    sesLive[c] => registry[sesDevice[c]] = "active"

RevocationIrreversible ==
  \A d \in revokedEver : registry[d] = "revoked"

OwnershipImmobile ==
  \A s \in Sources : Cardinality(srcEverOwned[s]) =< 1

OwnerIsRegistrar ==
  \A s \in Sources :
    srcOwner[s] # None => srcEverOwned[s] = {srcOwner[s]}

AnnotationWriterFromSession ==
  \A a \in Annotations, d \in Devices :
    annRowSes[a][d] # None => sesDevice[annRowSes[a][d]] = d

AnnotationRowTouched ==
  \A a \in Annotations, d \in Devices :
    d \in annRows[a] => annRowSes[a][d] # None

Quiescent ==
  /\ \A i \in Invitations : invState[i] # "open"
  /\ \A d \in Devices : registry[d] # "active"
  /\ \A c \in Sessions : ~sesLive[c]

====
