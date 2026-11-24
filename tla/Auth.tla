---- MODULE Auth ----

EXTENDS Naturals, FiniteSets

CONSTANTS
  Devices,
  Invitations,
  Sessions,
  Sources,
  Annotations,
  Workspaces,
  Repos,
  MaxPurges

VARIABLES
  invState,
  invUsedBy,
  registry,
  pairedVia,
  revokedEver,
  sesDevice,
  sesLive,
  srcReg,
  srcPurges,
  srcReallows,
  srcOwner,
  srcEverOwned,
  srcWs,
  srcWsWriter,
  annRows,
  annRowSes,
  wsObs,
  wsWriter

vars == <<invState, invUsedBy, registry, pairedVia, revokedEver,
          sesDevice, sesLive, srcReg, srcPurges, srcReallows,
          srcOwner, srcEverOwned, srcWs, srcWsWriter, annRows, annRowSes, wsObs, wsWriter>>

None == "none"

InvStateVal == {"unissued", "open", "used", "expired"}
RegStateVal  == {"absent", "active", "revoked"}
SrcRegVal    == {"registered", "purged"}

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
  /\ srcReg       = [s \in Sources |-> "registered"]
  /\ srcPurges    = [s \in Sources |-> 0]
  /\ srcReallows  = [s \in Sources |-> 0]
  /\ srcOwner     = [s \in Sources |-> None]
  /\ srcEverOwned = [s \in Sources |-> {}]
  /\ srcWs        = [s \in Sources |-> None]
  /\ srcWsWriter  = [s \in Sources |-> None]
  /\ annRows      = [a \in Annotations |-> {}]
  /\ annRowSes    = [a \in Annotations |-> [d \in Devices |-> None]]
  /\ wsObs        = [d \in Devices |-> [w \in Workspaces |-> None]]
  /\ wsWriter     = [d \in Devices |-> [w \in Workspaces |-> None]]

IssueInvitation(i) ==
  /\ invState[i] = "unissued"
  /\ invState'  = [invState EXCEPT ![i] = "open"]
  /\ UNCHANGED <<wsObs, wsWriter, invUsedBy, registry, pairedVia, revokedEver, sesDevice,
                 sesLive, srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned, srcWs,
                 srcWsWriter, annRows, annRowSes>>

ExpireInvitation(i) ==
  /\ invState[i] = "open"
  /\ invState'  = [invState EXCEPT ![i] = "expired"]
  /\ UNCHANGED <<wsObs, wsWriter, invUsedBy, registry, pairedVia, revokedEver, sesDevice,
                 sesLive, srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned, srcWs,
                 srcWsWriter, annRows, annRowSes>>

ConsumeInvitation(d, i) ==
  /\ invState[i] = "open"
  /\ registry[d] = "absent"
  /\ invState'  = [invState EXCEPT ![i] = "used"]
  /\ invUsedBy' = [invUsedBy EXCEPT ![i] = d]
  /\ registry'  = [registry EXCEPT ![d] = "active"]
  /\ pairedVia' = [pairedVia EXCEPT ![d] = i]
  /\ UNCHANGED <<wsObs, wsWriter, revokedEver, sesDevice, sesLive, srcReg,
                 srcPurges, srcReallows, srcOwner, srcEverOwned, srcWs, srcWsWriter,
                 annRows, annRowSes>>

RevokeDevice(d) ==
  /\ registry[d] = "active"
  /\ registry'   = [registry EXCEPT ![d] = "revoked"]
  /\ revokedEver' = revokedEver \cup {d}
  /\ sesLive'    = [c \in Sessions |->
                      IF sesLive[c] /\ sesDevice[c] = d
                        THEN FALSE
                        ELSE sesLive[c]]
  /\ UNCHANGED <<wsObs, wsWriter, invState, invUsedBy, pairedVia, sesDevice,
                 srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned, srcWs, srcWsWriter,
                 annRows, annRowSes>>

OpenSession(d, c) ==
  /\ registry[d] = "active"
  /\ sesDevice[c] = None
  /\ sesDevice'  = [sesDevice EXCEPT ![c] = d]
  /\ sesLive'    = [sesLive EXCEPT ![c] = TRUE]
  /\ UNCHANGED <<wsObs, wsWriter, invState, invUsedBy, registry, pairedVia, revokedEver,
                 srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned, srcWs, srcWsWriter,
                 annRows, annRowSes>>

CloseSession(c) ==
  /\ sesLive[c]
  /\ sesLive' = [sesLive EXCEPT ![c] = FALSE]
  /\ UNCHANGED <<wsObs, wsWriter, invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned, srcWs,
                 srcWsWriter, annRows, annRowSes>>

Upload(c, s) ==
  /\ Authorized(c)
  /\ srcReg[s] = "registered"
  /\ LET d == sesDevice[c]
     IN /\ srcOwner[s] \in {None, d}
        /\ srcOwner'     = [srcOwner EXCEPT ![s] = d]
        /\ srcEverOwned' = [srcEverOwned EXCEPT ![s] = srcEverOwned[s] \cup {d}]
  /\ UNCHANGED <<wsObs, wsWriter, invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, srcReg, srcPurges, srcReallows, srcWs, srcWsWriter,
                 annRows, annRowSes>>

Annotate(c, a) ==
  /\ Authorized(c)
  /\ LET d == sesDevice[c]
     IN /\ annRows'   = [annRows EXCEPT ![a] = annRows[a] \cup {d}]
        /\ annRowSes' = [annRowSes EXCEPT ![a] =
                           [annRowSes[a] EXCEPT ![d] = c]]
  /\ UNCHANGED <<wsObs, wsWriter, invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned,
                 srcWs, srcWsWriter>>

DeleteAnnotation(c, a) ==
  /\ Authorized(c)
  /\ LET d == sesDevice[c]
     IN /\ annRows'   = [annRows EXCEPT ![a] = annRows[a] \ {d}]
        /\ annRowSes' = [annRowSes EXCEPT ![a] =
                           [annRowSes[a] EXCEPT ![d] = c]]
  /\ UNCHANGED <<wsObs, wsWriter, invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned,
                 srcWs, srcWsWriter>>

Observe(c, w, r) ==
  /\ Authorized(c)
  /\ LET d == sesDevice[c]
     IN IF wsObs[d][w] = None
          THEN /\ wsObs'    = [wsObs EXCEPT ![d][w] = r]
               /\ wsWriter' = [wsWriter EXCEPT ![d][w] = c]
          ELSE UNCHANGED <<wsObs, wsWriter>>
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned,
                 srcWs, srcWsWriter, annRows, annRowSes>>

SetSourceWorkspace(c, s, p) ==
  /\ Authorized(c)
  /\ srcReg[s] = "registered"
  /\ srcOwner[s] = sesDevice[c]
  /\ IF srcWs[s] = None
       THEN /\ srcWs'       = [srcWs EXCEPT ![s] = p]
            /\ srcWsWriter' = [srcWsWriter EXCEPT ![s] = c]
       ELSE UNCHANGED <<srcWs, srcWsWriter>>
  /\ UNCHANGED <<wsObs, wsWriter, invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, srcReg, srcPurges, srcReallows, srcOwner, srcEverOwned,
                 annRows, annRowSes>>

PurgeSource(s) ==
  /\ srcPurges[s] < MaxPurges
  /\ srcReg[s] = "registered"
  /\ srcReg'    = [srcReg EXCEPT ![s] = "purged"]
  /\ srcPurges' = [srcPurges EXCEPT ![s] = srcPurges[s] + 1]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver, sesDevice,
                 sesLive, srcReallows, srcOwner, srcEverOwned, srcWs, srcWsWriter,
                 annRows, annRowSes, wsObs, wsWriter>>

ReallowSource(s) ==
  /\ srcReg[s] = "purged"
  /\ srcReallows[s] < srcPurges[s]
  /\ srcReg'      = [srcReg EXCEPT ![s] = "registered"]
  /\ srcReallows' = [srcReallows EXCEPT ![s] = srcReallows[s] + 1]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver, sesDevice,
                 sesLive, srcPurges, srcOwner, srcEverOwned, srcWs, srcWsWriter,
                 annRows, annRowSes, wsObs, wsWriter>>

ReadDiagnostics(c) ==
  /\ Authorized(c)
  /\ UNCHANGED vars

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
       \/ CloseSession(c)
       \/ ReadDiagnostics(c)
  \/ \E s \in Sources :
       \/ PurgeSource(s)
       \/ ReallowSource(s)
  \/ \E c \in Sessions, s \in Sources :
       Upload(c, s)
  \/ \E c \in Sessions, a \in Annotations :
       \/ Annotate(c, a)
       \/ DeleteAnnotation(c, a)
  \/ \E c \in Sessions, w \in Workspaces, r \in Repos :
       Observe(c, w, r)
  \/ \E c \in Sessions, s \in Sources, p \in Workspaces :
       SetSourceWorkspace(c, s, p)

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ invState     \in [Invitations -> InvStateVal]
  /\ invUsedBy    \in [Invitations -> Devices \cup {None}]
  /\ registry     \in [Devices -> RegStateVal]
  /\ pairedVia    \in [Devices -> Invitations \cup {None}]
  /\ revokedEver  \in SUBSET Devices
  /\ sesDevice    \in [Sessions -> Devices \cup {None}]
  /\ sesLive      \in [Sessions -> BOOLEAN]
  /\ srcReg       \in [Sources -> SrcRegVal]
  /\ srcPurges    \in [Sources -> 0..MaxPurges]
  /\ srcReallows  \in [Sources -> 0..MaxPurges]
  /\ srcOwner     \in [Sources -> Devices \cup {None}]
  /\ srcEverOwned \in [Sources -> SUBSET Devices]
  /\ srcWs        \in [Sources -> Workspaces \cup {None}]
  /\ srcWsWriter  \in [Sources -> Sessions \cup {None}]
  /\ annRows      \in [Annotations -> SUBSET Devices]
  /\ annRowSes    \in [Annotations -> [Devices -> Sessions \cup {None}]]
  /\ wsObs        \in [Devices -> [Workspaces -> Repos \cup {None}]]
  /\ wsWriter     \in [Devices -> [Workspaces -> Sessions \cup {None}]]

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

TombstoneMatchesOwnerActions ==
  \A s \in Sources :
    (srcReg[s] = "purged") = (srcPurges[s] > srcReallows[s])

OwnerIsRegistrar ==
  \A s \in Sources :
    srcOwner[s] # None => srcEverOwned[s] = {srcOwner[s]}

AnnotationWriterFromSession ==
  \A a \in Annotations, d \in Devices :
    annRowSes[a][d] # None => sesDevice[annRowSes[a][d]] = d

AnnotationRowTouched ==
  \A a \in Annotations, d \in Devices :
    d \in annRows[a] => annRowSes[a][d] # None

ObservationWriterFromSession ==
  \A d \in Devices, w \in Workspaces :
    wsWriter[d][w] # None => sesDevice[wsWriter[d][w]] = d

ObservationHasWriter ==
  \A d \in Devices, w \in Workspaces :
    (wsObs[d][w] # None) = (wsWriter[d][w] # None)

ObservationWriteOnce ==
  [][\A d \in Devices, w \in Workspaces :
       wsObs[d][w] # None =>
         /\ wsObs'[d][w] = wsObs[d][w]
         /\ wsWriter'[d][w] = wsWriter[d][w]]_vars

SourceWorkspaceWriterFromSession ==
  \A s \in Sources :
    srcWsWriter[s] # None =>
      /\ srcOwner[s] # None
      /\ sesDevice[srcWsWriter[s]] = srcOwner[s]

SourceWorkspaceHasWriter ==
  \A s \in Sources :
    (srcWs[s] # None) = (srcWsWriter[s] # None)

SourceWorkspaceWriteOnce ==
  [][\A s \in Sources :
       srcWs[s] # None =>
         /\ srcWs'[s] = srcWs[s]
         /\ srcWsWriter'[s] = srcWsWriter[s]]_vars

Quiescent ==
  /\ \A i \in Invitations : invState[i] # "open"
  /\ \A d \in Devices : registry[d] # "active"
  /\ \A c \in Sessions : ~sesLive[c]

====
