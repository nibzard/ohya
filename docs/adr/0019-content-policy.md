# 0019: Content policy at egress and annotation

Status: accepted, 2026-11-27 (T155). Extends
[ADR 0015](0015-egress-secret-redaction.md).

## Context

Stored transcripts are untrusted. Beyond credentials, they can hold
characters that disguise text (bidi controls) and sentences that read as
instructions to a model. A competitive study of nicknisi/sessions
(2026-11-27) showed those two classes withheld at every memory boundary.
ohya keeps raw records byte for byte, so the boundary stays at egress.

## Decision

1. **Hidden characters are replaced at egress.** The `invisible-unicode`
   pattern joins the table in `internal/secrets`: bidi controls
   U+202A–202E and U+2066–2069, U+2060, and U+200B. The existing
   marker mechanism and `redactions` count apply.
2. **U+200C, U+200D, and U+FEFF are not matched.** They are part of emoji
   sequences, Persian and Indic text, and byte order marks. Tests pin an
   emoji sequence and a Persian word.
3. **Injection phrasing is flagged, not replaced.** A fixed anchored
   pattern ("ignore/disregard/forget" plus previous, prior, above,
   earlier, or preceding plus instructions, prompts, directions, or
   rules; and "disregard all prior") increments an additive
   `injection_flags` count on the result. The text is served unchanged.
4. **`annotate` rejects both classes.** Annotation values are authored
   and small. The check decodes the JSON value, so `‮` escapes are
   caught. The error names the rule. Secrets in annotation values stay
   served through egress redaction as before.

## Measurement (C1d)

On the owner's hub, FTS counts for the candidate phrases on 2026-11-27:
"ignore previous instructions" matched 161 events in about 110 sessions,
mostly Codex tool output, and "disregard all prior" matched 11. ohya's own
sessions discuss injection, so a replacement would hide those events from
owner audits. That rate decided item 3.

## Consequences

- Agents see a count and the server instructions already say to treat
  transcript text as data. The count lets an agent or script notice.
- A pattern false positive costs a count, not hidden text.
- `ohya secrets scan` reports `invisible-unicode` beside the secret ids.
- No sync or authorization state changes.
