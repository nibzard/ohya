# ohya — Engineering Manifesto

**Version 1.0 · Principles for building simple, reliable, enduring software**

## Our Philosophy

We build software that is **simple to understand, difficult to break, easy to extend, and inexpensive to operate**.

ohya is not an exercise in architectural sophistication. It is an exercise in engineering discipline. Our goal is to deliver a small, exceptionally reliable system that solves one problem extremely well.

We favor correctness over cleverness, explicitness over magic, and proven technology over novelty.

**Complexity is a liability. Every abstraction, dependency, feature, and line of code must earn its existence.**

---

## I. Foundational Principles

### 1. KISS — Simplicity Is the Highest Form of Engineering

Choose the simplest solution that fully satisfies the requirements.

- Prefer straightforward code over clever abstractions.
- Minimize dependencies, services, configuration, and moving parts.
- Avoid frameworks when a small library or standard-library solution suffices.
- Optimize for the engineer who will maintain the code six months from now.
- Delete complexity whenever possible instead of documenting around it.

**Rule:** If two designs solve the same problem, choose the one with fewer concepts and failure modes.

### 2. Domain-Driven Design — Model the Problem, Not the Infrastructure

Use Domain-Driven Design (DDD) to establish precise domain boundaries and a shared language.

Our core concepts include Device, Source, Session, Event, Cursor, Sync Batch, and Acknowledgment.

- Establish a ubiquitous language across code, documentation, and tests.
- Define explicit domain invariants and ownership.
- Keep domain logic independent of transport, storage, and external SDKs.
- Organize modules around cohesive responsibilities.
- Apply bounded contexts where real domain boundaries exist.

DDD does not require elaborate enterprise patterns. Use its modeling discipline without unnecessary repositories, factories, interfaces, or abstraction layers.

**Rule:** The domain should be understandable without knowing which database or networking library we use.

### 3. Formal Methods — Prove Critical Behavior Before Trusting It

Use **TLA+ and the TLC model checker** to validate critical distributed-system behavior.

Prioritize properties that are difficult to establish through conventional tests:

- Events are not lost after acknowledged commits.
- Retries and reconnections do not create duplicate events.
- Cursor advancement never precedes durable storage.
- Crashes and network partitions preserve consistency.
- Concurrent synchronization does not violate ordering guarantees.
- Revoked devices cannot establish new authorized sessions.

Define safety properties, liveness properties, assumptions, and failure scenarios explicitly.

Model state transitions before implementing complex synchronization behavior. Keep specifications small, reviewable, and executable in CI where practical.

Formal verification supplements integration and fault-injection testing; it does not replace them or prove the entire implementation correct.

**Rule:** Model-check the protocols where a subtle mistake can cause irreversible data loss or corruption.

### 4. Modularity — Strong Boundaries, Minimal Coupling

Build a modular monolith, not a distributed collection of services.

- One Go binary with clearly separated internal modules.
- Isolate ingestion, domain normalization, synchronization, persistence, transport, and MCP.
- Keep interfaces small, explicit, and defined where they are consumed.
- Prefer dependency injection through constructors over global state.
- Ensure core logic can be tested without networking or a running service.
- Allow implementations to change without modifying unrelated modules.

A module should have one clear responsibility and expose the smallest useful surface.

**Rule:** Modules exist to contain complexity, not to create ceremony.

### 5. DRY — One Source of Truth

Avoid duplicating knowledge, business rules, schemas, and invariants.

- Centralize domain definitions and validation rules.
- Reuse proven components where duplication would create drift.
- Generate repetitive artifacts from authoritative definitions when worthwhile.
- Keep database schema and migrations versioned.
- Prefer SQL for data operations rather than recreating database capabilities in application code.

But do not abstract merely because two pieces of code look similar.

**Rule:** Duplicate stable code when necessary; never duplicate knowledge that must remain consistent.

### 6. YAGNI — Build Only What We Need

We do not build speculative features.

Every feature must address a real user need, protect a system invariant, or meaningfully improve reliability or maintainability.

For ohya, this means:

- SQLite before distributed databases.
- FTS5/BM25 before vector search.
- One SQL MCP tool before specialized retrieval APIs.
- Embedded Tailcat before custom networking infrastructure.
- Deterministic metadata extraction before LLM pipelines.
- Agent-side reasoning before adding classification infrastructure.

Do not build plugin systems, compatibility layers, or general-purpose frameworks for hypothetical future requirements.

**Rule:** A feature we never build has zero maintenance cost.

---

## II. Correctness, Performance, and Reliability

### 7. Correctness Is Non-Negotiable

Reliability is a core product feature, not an operational afterthought.

- Define invariants before implementation.
- Make ingestion idempotent and retry-safe.
- Use transactional boundaries deliberately.
- Never acknowledge data before durable commit.
- Preserve source records for debugging and reprocessing.
- Design explicitly for offline operation, interruption, and recovery.
- Make schema migrations safe and testable.

Favor predictable state transitions over implicit behavior.

**Rule:** No silent data loss, no silent corruption, no false acknowledgment of success.

### 8. Performance by Design, Not by Guesswork

ohya must feel instantaneous while remaining lightweight.

- Keep ingestion incremental and avoid unnecessary file rescans.
- Push newly written records without blocking coding agents.
- Perform indexed queries close to the data.
- Bound memory, concurrency, query execution, and response sizes.
- Avoid expensive processing on critical paths.
- Use benchmarks and profiling before introducing optimizations.
- Measure CPU, memory, database size, ingestion latency, and query latency.

Prefer algorithmic simplicity and good data structures over sophisticated caching.

**Rule:** Benchmark before optimizing. Optimize without sacrificing clarity or correctness.

### 9. Security and Privacy by Default

Coding sessions can contain proprietary code, credentials, and sensitive context.

- Keep the database under user control.
- Encrypt device communication and authenticate peers.
- Support device revocation and secure credential storage.
- Enforce read-only SQL access through multiple safeguards.
- Treat all transcripts, tool outputs, and model-generated queries as untrusted input.
- Apply least privilege at every boundary.
- Never transmit session content to third-party AI services without explicit opt-in.
- Avoid leaking sensitive transcript content through logs, errors, or telemetry.

Security must be built into the architecture rather than layered on later.

**Rule:** Local-first is a security commitment, not just a deployment preference.

### 10. Failure Is Normal — Recovery Must Be Automatic

Devices sleep. Networks partition. Processes crash. Logs rotate. Databases encounter contention.

These are expected operating conditions.

Design every subsystem with clear recovery behavior. Prefer persisted state, idempotent operations, bounded retries, and explicit error handling.

A system that requires manual intervention after ordinary failures is unfinished.

**Rule:** The happy path matters. The recovery path matters equally.

---

## III. Development Discipline

### 11. Documentation Is a First-Class Deliverable

**Documentation is part of the implementation, not a task postponed until after it.**

Maintain concise, accurate documentation covering:

- **README:** What ohya is, how to install it, and how to use it.
- **Architecture:** Components, boundaries, data flows, and key decisions.
- **Domain model:** Terminology, entities, lifecycle, and invariants.
- **Database:** Schema, indexes, migrations, and example SQL.
- **Protocols:** Synchronization, pairing, authentication, and failure semantics.
- **Formal specifications:** TLA+ models, properties, and assumptions.
- **ADRs:** Important architectural decisions and rejected alternatives.
- **Operations:** Troubleshooting, recovery, backup, and upgrade procedures.
- **Development:** How to build, test, benchmark, and contribute.

Document *why* decisions were made, not merely *what* the code does.

Keep documentation close to the code and update it in the same pull request as the relevant behavior change.

**Rule:** If a behavior is important enough to implement, it is important enough to explain.

### 12. Testing Is Design Validation

Testing should establish confidence in behavior, not merely increase coverage statistics.

Use complementary techniques:

- Unit tests for domain logic and parsing.
- Table-driven tests for agent-specific formats.
- Property-based and fuzz tests for parsers and protocol inputs.
- SQLite integration tests for persistence and migrations.
- End-to-end tests across multiple simulated devices.
- Fault injection for network failures, retries, process crashes, and interrupted commits.
- TLA+ model checking for critical state machines.
- Performance regression tests for ingestion and retrieval.

Every meaningful bug fix should include a regression test.

**Rule:** Test externally observable behavior and invariants, not private implementation details.

### 13. Explicitness Over Hidden Behavior

Avoid implicit side effects, hidden global state, automatic magic, and ambiguous error handling.

- Make ownership of data and resources explicit.
- Favor deterministic and reproducible behavior.
- Use clear names and small functions.
- Return meaningful errors with sufficient context.
- Maintain clear lifecycle and concurrency rules.
- Make background work observable and optional where appropriate.

Prefer a few understandable functions over a complicated abstraction.

**Rule:** A developer should be able to trace what happens to an event from source file to SQL query without guessing.

### 14. Dependencies Are Architectural Decisions

Every dependency increases the product's operational and maintenance surface.

- Prefer Go's standard library.
- Choose small, focused, actively maintained libraries.
- Pin dependencies and track compatibility.
- Isolate experimental APIs, especially transport dependencies.
- Avoid unnecessary build tooling and runtime requirements.
- Preserve reproducible builds and cross-platform behavior.

Do not reinvent SQLite, cryptography, or networking protocols. But do not import a framework to avoid writing twenty lines of clear code.

**Rule:** Minimize dependency count and dependency risk, not merely lines of application code.

### 15. Evolve Through Small, Reviewable Changes

Keep development incremental.

Each change should have a clear purpose, bounded scope, documented implications, and tests appropriate to its risks.

Prefer short feedback cycles, continuous integration, automated formatting, static analysis, and reproducible test environments.

Use ADRs for significant design choices. Revisit decisions when new evidence emerges without destabilizing working foundations.

**Rule:** Every pull request should leave the system simpler, safer, or more useful.

---

## IV. Architectural Commitments

The following decisions define ohya's initial architectural direction.

| Concern | Commitment |
|---|---|
| Runtime | Go, one compiled executable |
| Architecture | Modular monolith |
| Persistence | Single SQLite database with WAL |
| Data model | Sessions, events, sources, and metadata |
| Search | SQLite FTS5 with native BM25 |
| Synchronization | Incremental, idempotent, acknowledged |
| Networking | Embedded Tailcat, isolated behind a transport boundary |
| Agent integration | One read-only SQL MCP tool |
| Intelligence | Agent-side reasoning |
| Classification | Deferred beyond MVP |
| Correctness | Explicit invariants, TLA+, tests, fault injection |
| Deployment | Minimal configuration, no mandatory cloud services |

These are deliberate choices, not permanent dogma. Change them only when evidence demonstrates that the alternative delivers meaningful value.

---

## V. Definition of Done

A feature is complete only when:

1. **The behavior is correct** and its relevant invariants are preserved.
2. **The implementation is simple** and respects domain and module boundaries.
3. **Tests pass**, including appropriate failure and edge cases.
4. **Critical protocol changes are model-checked** against their TLA+ specifications.
5. **Documentation is updated**, including architecture decisions where necessary.
6. **Performance is understood** and critical paths show no unacceptable regressions.
7. **Security and privacy implications are reviewed.**
8. **Recovery behavior is defined**, and failures are observable.
9. **No unnecessary dependencies, abstractions, or configuration have been introduced.**
10. **Another engineer can understand and maintain the change without its original author.**

---

## VI. Our Decision Framework

When facing competing designs, ask:

1. Which option has fewer moving parts?
2. Which option is easier to reason about and verify?
3. Which option preserves domain boundaries and invariants?
4. Which option fails more predictably and recovers more safely?
5. Which option requires less ongoing maintenance?
6. Can we accomplish the same result using capabilities we already have?
7. Can we defer this complexity until there is evidence we need it?

When these considerations conflict, prioritize:

**Correctness → Security → Simplicity → Reliability → User Experience → Performance → Extensibility**

Performance matters enormously, but not at the expense of correctness. Extensibility matters, but speculative extensibility must never compromise simplicity.

---

## VII. The ohya Engineering Promise

We will not mistake complexity for sophistication.

We will not introduce infrastructure without necessity.

We will not use AI where deterministic code is sufficient.

We will not sacrifice correctness for speed of implementation.

We will not create abstractions before understanding the domain.

We will not consider undocumented, untested behavior finished.

We will build software that is small enough to understand completely, reliable enough to trust every day, and modular enough to evolve without rewriting its foundations.

**Our north star:**

> The best architecture is the smallest one that can be understood, verified, operated, and trusted.

**Build less. Prove more. Make it last.**
