# 0020: CJK search

Status: proposed, 2026-11-27 (T157). Waits for the owner to adopt it.

## Context

`events_fts` tokenizes with `porter unicode61`
([migration 012](../../internal/store/migrations/012_fts_porter.sql)).
`unicode61` has no word dictionary, so a run of Chinese or Japanese
characters is one token and a search for a word inside the run finds
nothing. Korean is space-separated and already works. The sessions study
(2026-11-27) ships a two-character side index for this.

## Spike (F2a)

20,000 synthetic documents: English paragraphs from this repository, and
20 percent with Japanese, Chinese, or Korean sentences added. Python 3 with
SQLite 3.45.1. The script is in `/notes/2026-11-27-cjk-search-spike.md`.

| Index | Size | Two-character CJK word | Three or more characters | Single character |
|---|---|---|---|---|
| `porter unicode61` (today) | 9.0 MB | no match | no match for Japanese and Chinese | no match |
| `trigram` over all text | 24.1 MB | no match (needs three characters) | exact | no match |
| Bigram side table, CJK runs only | 1.5 MB | exact | exact (phrase of bigrams) | no match inside a run |

The trigram index grows the whole index 2.7 times, because it indexes
English too. It also drops stemming and cannot answer two-character
Japanese and Chinese words, which are the common length. The bigram table
holds only CJK runs, so its size follows the CJK content.

## Decision (recommended)

Adopt a bigram side table, `events_cjk_fts`, only if the owner searches
CJK history. Do not adopt trigram.

- **Index:** external-content FTS5 table over a derived column of
  space-separated CJK bigrams, `unicode61` tokenizer.
- **Query:** the agent converts a CJK word to bigrams and runs a phrase
  `MATCH` on `events_cjk_fts`. A recipe in `DB.md` and the skill shows
  this. English search stays on `events_fts`.
- **Maintenance cost:** SQLite triggers cannot compute bigrams. The
  writer must derive them in Go at insert time and in the migration's
  backfill, which scans every event. This is the main cost: one more
  derived value to keep in step with `events.text`, plus re-derive support
  in `store.ReDerive`.

## Consequences

- Adoption needs migration 013, an insert-path change, a re-derive change,
  a recipe, and a bench run. Plan 006 item F2c.
- Not adopting leaves CJK search matching only space-separated text.
- The corpus evidence of demand is unmeasured. `unicode61` stores CJK runs
  as single tokens, so a count over `events_fts` cannot size the CJK
  content. The owner decides.
- No sync or authorization state changes.
