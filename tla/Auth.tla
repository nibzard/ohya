(***************************************************************************)
(* ohya — authorization, pairing, and revocation.                          *)
(*                                                                         *)
(* This module is the executable TLA+ model of the authorization rules in  *)
(* ARCHITECTURE.md ("Authorization and SQL", "Agent annotations"): the     *)
(* hub-owned device credential registry, one-time expiring pairing         *)
(* invitations, per-request revocation checks, the device ownership of     *)
(* sources, and the writer ownership of annotations.                       *)
(*                                                                         *)
(* It decides who may act. Sync.tla decides what a committed batch does    *)
(* to cursors, under its fixed-ownership assumption; this module is that   *)
(* assumption, checked.                                                    *)
(*                                                                         *)
(* Safety properties, checked by every configuration:                      *)
(*   - a revoked device cannot establish a new authorized session, and     *)
(*     revocation is final for the device identity;                        *)
(*   - an invitation is consumed at most once, and only before expiry;     *)
(*   - source ownership cannot move between devices;                       *)
(*   - an annotation's writer is the session's device, never the request.  *)
(*                                                                         *)
(* Scope and assumptions are recorded in tla/README.md. The companion      *)
(* configurations check this spec with TLC:                                *)
(*   Auth.cfg      — two devices racing for two invitations                *)
(*   AuthWide.cfg  — three devices, two invitations: pairing scarcity      *)
(***************************************************************************)

---- MODULE Auth ----

EXTENDS Naturals, FiniteSets

CONSTANTS
  Devices,      \* device identity strings, e.g. {"d1", "d2"}
  Invitations,  \* invitation identity strings
  Sessions,     \* session (connection) slot strings
  Sources,      \* source identity strings
  Annotations   \* annotation slots: the (event_id, namespace, key) part of
                 \* the annotation identity; the writer_id part is state
                 \* (the row owner), not a constant

VARIABLES
  \* The invitation's lifecycle. "open" is the only consumable state; a
  \* consumed invitation is permanently "used", an expired one permanently
  \* "expired". Expiry is modeled as a nondeterministic action, not a
  \* clock: the protocol property is that consumption must race expiry
  \* and lose, and the TTL length is configuration, not protocol.
  invState,
  \* The device that consumed each invitation. Only meaningful for
  \* "used"; the invariant below ties the two together.
  invUsedBy,
  \* The hub-owned device credential registry: the authorization fact per
  \* device. "absent" devices are strangers, "active" devices are paired,
  \* "revoked" devices are expelled. The credential secret itself is an
  \* assumption (unguessable, see README), not modeled state.
  registry,
  \* The invitation each device consumed to pair. Set once, never changed.
  pairedVia,
  \* Devices that were revoked at any time. Revocation is one-way: it is
  \* history, so the invariants can state that a revoked identity never
  \* becomes authorized again, not even through a fresh invitation.
  revokedEver,
  \* Each session slot's device. A slot is single-use: None until a
  \* device opens it, then bound forever, even after close — the binding
  \* is what annotation history refers to, so it must survive.
  sesDevice,
  \* Whether a session slot currently holds a live session. Revocation
  \* closes the device's live sessions; the slot stays bound to it.
  sesLive,
  \* The device each source is bound to. Binds at the first accepted
  \* upload; None before that. Never moves, never unbinds — a revoked
  \* device's sources stay locked rather than rebindable.
  srcOwner,
  \* Every device a source was ever bound to. Pure history: it grows
  \* only at binding, so "at most one registrar" is checkable as a state
  \* invariant even though a move would otherwise leave no trace.
  srcEverOwned,
  \* The devices holding a live annotation row on each slot. Several
  \* devices may write the same slot; each owns its own row.
  annRows,
  \* The session through which each row was last written or deleted.
  \* Pure history again: it pins the row's writer to a session of that
  \* same device, which is the "writer comes from the session, never
  \* from caller input" rule, checkable.
  annRowSes

vars == <<invState, invUsedBy, registry, pairedVia, revokedEver,
          sesDevice, sesLive, srcOwner, srcEverOwned, annRows, annRowSes>>

(* One sentinel string for "no device", "no invitation", and "no         *)
(* session": the domains are disjoint and no predicate compares          *)
(* sentinels across domains, so one sentinel keeps the value sets small. *)
None == "none"

InvStateVal == {"unissued", "open", "used", "expired"}
RegStateVal  == {"absent", "active", "revoked"}

(* Every request re-checks the registry: ARCHITECTURE.md says "check     *)
(* revocation on every request". In this model the check is redundant    *)
(* with sesLive, because RevokeDevice closes the device's sessions       *)
(* atomically. It stays anyway: it is the rule the implementation must   *)
(* apply per request, where teardown of an open connection cannot be     *)
(* atomic with a request already being processed.                        *)
Authorized(c) ==
  /\ sesLive[c]
  /\ registry[sesDevice[c]] = "active"

(***********************************************************************)
(* Initial state. Nothing is issued, no device is known to the hub, no  *)
(* session exists, no source is bound, no annotation row exists. The    *)
(* operator and the devices arrive through the actions below.           *)
(***********************************************************************)

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

(***********************************************************************)
(* Pairing: the operator issues and expires invitations, a device       *)
(* consumes one, and the hub admits the device into the registry.       *)
(***********************************************************************)

(* The operator issues an invitation. Single-use and expiring by        *)
(* construction: it may be consumed once, and it may expire at any      *)
(* moment, before or after any device attempt.                          *)
IssueInvitation(i) ==
  /\ invState[i] = "unissued"
  /\ invState'  = [invState EXCEPT ![i] = "open"]
  /\ UNCHANGED <<invUsedBy, registry, pairedVia, revokedEver, sesDevice,
                 sesLive, srcOwner, srcEverOwned, annRows, annRowSes>>

(* The invitation's lifetime ends before consumption. Nondeterministic  *)
(* and unfair: a device holding a valid invitation may still lose the   *)
(* race, which is the behavior "expiring" must tolerate.                *)
ExpireInvitation(i) ==
  /\ invState[i] = "open"
  /\ invState'  = [invState EXCEPT ![i] = "expired"]
  /\ UNCHANGED <<invUsedBy, registry, pairedVia, revokedEver, sesDevice,
                 sesLive, srcOwner, srcEverOwned, annRows, annRowSes>>

(* A device presents an invitation; the hub admits it. One atomic       *)
(* transaction consumes the invitation and registers the credential,    *)
(* which is what makes "one-time" hold: no window exists where the      *)
(* invitation is consumed but the device is not yet registered, or      *)
(* vice versa. A revoked identity cannot consume a fresh invitation:    *)
(* the guard requires "absent", and revocation never returns to it.     *)
(* Re-admission therefore requires a new device identity, not a new     *)
(* invitation (a design decision this model fixes; see README).         *)
ConsumeInvitation(d, i) ==
  /\ invState[i] = "open"
  /\ registry[d] = "absent"
  /\ invState'  = [invState EXCEPT ![i] = "used"]
  /\ invUsedBy' = [invUsedBy EXCEPT ![i] = d]
  /\ registry'  = [registry EXCEPT ![d] = "active"]
  /\ pairedVia' = [pairedVia EXCEPT ![d] = i]
  /\ UNCHANGED <<revokedEver, sesDevice, sesLive, srcOwner, srcEverOwned,
                 annRows, annRowSes>>

(***********************************************************************)
(* Revocation: final for the device identity.                           *)
(***********************************************************************)

(* The operator expels a device. The registry flips and, in the same    *)
(* step, every live session of that device closes and its in-flight     *)
(* requests are cancelled — the model makes teardown atomic, which the  *)
(* single hub writer serializes in the implementation. Data already     *)
(* returned to the device before this step cannot be recalled; that is  *)
(* an acknowledged limit, not a property. The device's sources stay     *)
(* bound and its annotation rows stay written; revocation stops new     *)
(* activity, it does not erase history.                                 *)
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

(***********************************************************************)
(* Sessions: a device establishes an authorized session, or ends one.   *)
(***********************************************************************)

(* Only an active device can open a session; the slot binds to it       *)
(* permanently. A revoked device's attempts fail the guard: this is     *)
(* the "cannot establish a new authorized session" property.            *)
OpenSession(d, c) ==
  /\ registry[d] = "active"
  /\ sesDevice[c] = None
  /\ sesDevice'  = [sesDevice EXCEPT ![c] = d]
  /\ sesLive'    = [sesLive EXCEPT ![c] = TRUE]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 srcOwner, srcEverOwned, annRows, annRowSes>>

(* The device or the hub closes a session. The binding survives; only   *)
(* a fresh slot can serve the device again.                             *)
CloseSession(c) ==
  /\ sesLive[c]
  /\ sesLive' = [sesLive EXCEPT ![c] = FALSE]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, srcOwner, srcEverOwned, annRows, annRowSes>>

(***********************************************************************)
(* Requests: what an authorized session may do. Rejection is the        *)
(* absence of a transition — a request whose authorization guard fails  *)
(* changes no state. Reads are not modeled: they carry no ownership     *)
(* state, and the same Authorized guard applies.                        *)
(***********************************************************************)

(* Upload a batch for source s. The first accepted upload binds the     *)
(* source to the session's device (the hub cannot know a source before  *)
(* its first batch); later uploads from the same device are accepted    *)
(* retries. Any other device's upload — stranger, paired, or the        *)
(* revoked former owner — fails the guard: ownership never moves.       *)
(* Batch content, cursors, and replay are Sync.tla's state machine.     *)
Upload(c, s) ==
  /\ Authorized(c)
  /\ LET d == sesDevice[c]
     IN /\ srcOwner[s] \in {None, d}
        /\ srcOwner'     = [srcOwner EXCEPT ![s] = d]
        /\ srcEverOwned' = [srcEverOwned EXCEPT ![s] = srcEverOwned[s] \cup {d}]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, annRows, annRowSes>>

(* Upsert the session device's own row on annotation slot a. The        *)
(* request carries no writer: the row addressed is (a, session device)  *)
(* by construction, so one device's upsert can never touch another's    *)
(* row, and the value semantics (atomic batch, identical-retry no-op)   *)
(* are Go-test territory (internal/store, T009).                        *)
Annotate(c, a) ==
  /\ Authorized(c)
  /\ LET d == sesDevice[c]
     IN /\ annRows'   = [annRows EXCEPT ![a] = annRows[a] \cup {d}]
        /\ annRowSes' = [annRowSes EXCEPT ![a] =
                           [annRowSes[a] EXCEPT ![d] = c]]
  /\ UNCHANGED <<invState, invUsedBy, registry, pairedVia, revokedEver,
                 sesDevice, sesLive, srcOwner, srcEverOwned>>

(* Delete the session device's own row on slot a. Deleting an absent    *)
(* row succeeds and changes nothing. Another device's row is not        *)
(* addressable: the guard and the effect name only the session's own    *)
(* row, which is the "may update or delete only their own               *)
(* annotations" rule.                                                   *)
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

(***********************************************************************)
(* Safety invariants.                                                   *)
(* Checked by every configuration below.                                *)
(***********************************************************************)

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

(* A live session always belongs to a bound device slot.                *)
SessionLiveHasDevice ==
  \A c \in Sessions : sesLive[c] => sesDevice[c] # None

(* An invitation cannot be consumed twice: at most one device ever      *)
(* paired through the same invitation.                                  *)
InvitationSingleUse ==
  \A i \in Invitations :
    Cardinality({d \in Devices : pairedVia[d] = i}) =< 1

(* The registry admits devices only through invitation consumption,     *)
(* and every consumed invitation registered its device: the registry   *)
(* entry and the pairing record exist together or not at all.          *)
PairingConsistent ==
  \A d \in Devices :
    (pairedVia[d] # None) = (registry[d] # "absent")

(* What the pairing record claims is true: the invitation is used and   *)
(* was used by exactly this device. A pairing through an expired or     *)
(* still-open invitation would violate this.                           *)
InvitationUsedByPairer ==
  \A d \in Devices :
    pairedVia[d] # None =>
      /\ invState[pairedVia[d]] = "used"
      /\ invUsedBy[pairedVia[d]] = d

(* A revoked device cannot establish a new authorized session: no live  *)
(* session belongs to a device that is not active. RevokeDevice's       *)
(* atomic close and OpenSession's guard maintain this together.         *)
NoLiveSessionForRevoked ==
  \A c \in Sessions :
    sesLive[c] => registry[sesDevice[c]] = "active"

(* Revocation is final for the device identity: a device revoked at     *)
(* any time is revoked now. Re-pairing with a fresh invitation cannot   *)
(* resurrect it; only a new device identity can join again.             *)
RevocationIrreversible ==
  \A d \in revokedEver : registry[d] = "revoked"

(* Source ownership cannot move between devices: a source is bound by   *)
(* at most one device ever. Orphaned sources of revoked devices stay    *)
(* locked, which is the conservative reading of the sync contract's    *)
(* "reject ... change source ownership".                               *)
OwnershipImmobile ==
  \A s \in Sources : Cardinality(srcEverOwned[s]) =< 1

(* The live owner is exactly the one registrar: no binding was          *)
(* overwritten, none appeared without registration.                     *)
OwnerIsRegistrar ==
  \A s \in Sources :
    srcOwner[s] # None => srcEverOwned[s] = {srcOwner[s]}

(* An annotation row's writer comes from the session, never from the    *)
(* request: every recorded write or delete went through a session       *)
(* bound to that same device. A caller-supplied writer_id breaks this.  *)
AnnotationWriterFromSession ==
  \A a \in Annotations, d \in Devices :
    annRowSes[a][d] # None => sesDevice[annRowSes[a][d]] = d

(* Every live row was written through some session of its writer.       *)
AnnotationRowTouched ==
  \A a \in Annotations, d \in Devices :
    d \in annRows[a] => annRowSes[a][d] # None

(***********************************************************************)
(* Helper for humans reading traces. Not checked.                       *)
(***********************************************************************)

(* The drained system: every invitation is dead, every paired device    *)
(* was revoked, no session is live. A legitimate fixed point, so TLC    *)
(* runs with -deadlock; a reported deadlock there is expected.          *)
Quiescent ==
  /\ \A i \in Invitations : invState[i] # "open"
  /\ \A d \in Devices : registry[d] # "active"
  /\ \A c \in Sessions : ~sesLive[c]

====
