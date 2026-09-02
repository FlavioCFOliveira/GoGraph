# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

GoGraph is a Go module for working with graphs: persistence, manipulation, and — above all — fast search. The API surface should stay small and ergonomic; performance is a first-class concern.

## Compliance Mandates

These four properties are non-negotiable invariants of the module. Every change — feature work, refactor, bug fix, performance tuning — must preserve them. A change that regresses any of them is not acceptable.

### 1. 100% Cypher TCK Compliant

The module is **100% compliant with the openCypher TCK** (Technology Compatibility Kit) at the execution level, as published at <https://opencypher.org/>. Every development must guarantee that the module remains 100% compatible with the openCypher specification.

- The full openCypher TCK execution suite is fully green: every scenario in `cypher/tck/features/` passes, with no `failed`, no `undefined`, and no `pending` steps.
- The regression gate in `cypher/tck/runner_test.go` (`const tckExecutionBaseline`) is set to the full scenario count. Any change that lowers the passing count fails `go test ./cypher/tck/...` — run locally as part of `make ci` before every push — and must not be merged.
- Conformance is evidence-based: do not claim openCypher behaviour from memory. When a question arises, consult the openCypher 9 specification, the relevant TCK feature file, or the upstream openCypher reference implementation before changing behaviour.
- New features that the openCypher TCK does not cover are allowed only when they do not conflict with any TCK-covered semantics.

### 2. 100% ACID Compliant

The module guarantees the **ACID** transactional properties — **Atomicity**, **Consistency**, **Isolation**, and **Durability** — across every feature, to provide **RELIABILITY** and **INTEGRITY** of stored data.

- **Atomicity** — every transaction is all-or-nothing: either every write becomes visible together or none of them do. Partial application after a crash or error is forbidden.
- **Consistency** — every committed transaction leaves the graph in a state that satisfies every declared invariant (schema constraints, uniqueness, label/property typing, referential integrity for edges, index correctness). Reads never observe a state that violates an invariant.
- **Isolation** — concurrent transactions behave as if serialised. Readers never observe the partial writes of an in-flight transaction; writers never silently overwrite each other.
- **Durability** — once a commit acknowledgement is returned, the change survives process crash, host crash, and `kill -9`. Verified by the deterministic crash-injection battery in `internal/crashinject/` and the WAL recovery tests in `store/wal/` and `store/recovery/`.

These properties must be preserved both for the in-memory engine and for every persistence backend. Any code path that could compromise an ACID property — a non-atomic multi-step write, a read that could observe partial state, a commit that does not durably flush — must be rejected at code review and must not be merged.

### 3. EXTREME / MASSIVE Concurrent Ready

The module is **designed and prepared to operate in an exemplary manner in production environments of extreme concurrency**. Concurrency is not a capability bolted on at the edges: it is a property of the design, present from the choice of every data structure to the shape of every public API.

- **Concurrent by design, never concurrent by adaptation.** Every component is designed from the outset for simultaneous use by a massive number of goroutines. A design that works only because callers serialise access externally does not satisfy this mandate.
- **Exemplary under sustained massive load.** Under heavy, prolonged, highly concurrent load the module must remain correct, stable, and predictable: no data race, no deadlock, no goroutine or memory leak, no unbounded growth, no latency cliff, and no crash. Saturation is answered with backpressure or a typed error, never with failure.
- **Scales with the hardware.** Throughput must rise with the cores made available to it. A hot path that serialises every caller on a single global lock is a defect against this mandate, not merely a missed optimisation — use sharding, atomics, or immutable snapshots instead.
- **Every concurrency contract is explicit.** Each exported type states in its godoc whether it is safe for concurrent use, and under which operations. Silence about a type's concurrency contract is a defect.
- **Proven, never presumed.** Readiness for extreme concurrency is established by measurement — race-detector runs, leak detection, and latency and throughput measured at the concurrency levels the module publishes (1, 8, 64, 256, 1024 goroutines) — never by inspection or by assertion.

The concrete, enforceable rules that implement this mandate are set out in [Reliability and Concurrency Mandates](#reliability-and-concurrency-mandates); that section is how the mandate is met, and this mandate is why the section exists.

### 4. ULTRA EFFICIENT by Design

The module uses, **by design, the minimum resources necessary** to perform its tasks. Wasteful use of resources must be fought at all times, in every component and on every code path.

- **Minimal by design, not by later optimisation.** The efficient structure is chosen when the component is designed, rather than a wasteful one being tuned afterwards. Efficiency is a design input, not a clean-up phase.
- **Every resource vector counts.** CPU, memory (RAM), storage (disk), and I/O are all in scope. A change that saves one vector by squandering another must be justified explicitly and measured, not assumed to be a net win.
- **Waste is a defect.** An avoidable allocation, a redundant copy, a needless recomputation, a byte written that need not be written, a goroutine that need not exist — each is a defect to be fixed like any other, never an acceptable cost of doing business.
- **Efficiency is measured, never claimed.** Every efficiency claim rests on evidence gathered in this project: benchmarks compared with `benchstat`, `pprof` CPU and heap profiles, allocation counts, and measured on-disk footprint. This is [Measure to decide](#measure-to-decide) applied to resource use.
- **Never at the expense of a higher priority.** Efficiency is pursued only once correctness and security are assured, exactly as the [Decision framework — correct → secure → fast](#decision-framework--correct--secure--fast) requires. Resources are never saved by weakening a guarantee.

The concrete, enforceable rules that implement this mandate are set out in [Performance-First Engineering](#performance-first-engineering); that section is how the mandate is met, and this mandate is why the section exists.

## Behavioural Rules

### Decision autonomy

You are **not authorised** to make decisions unilaterally. Whenever instructions are insufficient, unclear, non-specific, ambiguous, or contradictory, you **must always ask the user** how to proceed before taking any action.

**Boundary between acting and asking:** obvious, low-risk corrections — for example, a pre-existing bug with an unambiguous fix — proceed immediately. Any decision that changes the **scope**, the **expected behaviour**, the **architecture**, or the **requirements** must be put to the user first.

When asking for clarification:
- Present multiple options labelled `a)`, `b)`, `c)`, … and explicitly state which option you recommend.
- When there are multiple open questions, ask them **one at a time**, sequentially — never bundle several questions into a single prompt.

### Documentation language and quality

All project documentation must be written in **English**, at the highest standard: no spelling, grammar, or syntactic errors. Use clear, simple, unambiguous technical language aimed at human readers.

Documentation must be **accurate and faithful to the code** — never document intent, only what is actually implemented.

### Development workflow

Every piece of work must follow this exact sequence:

```
Specify → Implement → Test → Document
```

No step may be skipped or reordered.

### Git command execution

**Every `git` command is prepared and executed individually, under the coordination of the `gitflow` skill.** The skill owns the branching model and decides *which* git operation is correct for the situation; this section governs *how* each one is issued.

- **The `gitflow` skill coordinates every git operation.** Invoke it for the operations it owns — opening and closing a sprint's working branch, recording a closed task as a commit, cutting a release or a hotfix, merging, and any question about branch state relative to the gitflow model. Do not improvise a branching or merging decision that the skill exists to make.
- **The skill coordinates; it does not batch.** Whatever the skill prescribes is still issued one `git` invocation at a time, exactly as the rules below require. A plan produced by the skill is a sequence of individual executions, never a single compound one.

**Every `git` command is executed individually, on its own, and never together with any other command.** One `git` invocation per command execution — nothing before it, nothing after it.

- Never chain a `git` command with another command, by any means: no `&&`, no `||`, no `;`, no pipelines, no command substitution wrapping another command, no shell loops that run several commands in one invocation.
- Never chain two `git` commands together either. `git add` and `git commit` are two separate executions, run one after the other.
- This holds for every `git` subcommand — reads included (`git status`, `git log`, `git diff`, `git show`, `git blame`) as much as writes (`git add`, `git commit`, `git branch`, `git checkout`, `git merge`, `git tag`, `git push`).
- Redirecting or bounding a single `git` command's own output — for example `git log --oneline | head -20` — is the one permitted shaping of the invocation, because it runs no second command of its own.
- **Precedence.** This rule overrides the general guidance to group independent commands or tool calls into a single message (see [Token Economy](#token-economy)): saving a round trip never justifies combining a `git` command with anything else. Isolation makes each command's exit status unambiguous and keeps a failed step from being masked by the next one.

### Self-contained development

Every development cycle must be **self-contained**: never deliver only part of a task. Each cycle must produce a complete, usable result.

- When work uncovers a need that was not foreseen during planning, resolve it **within the same cycle** — add the necessary new tasks and complete them as promptly as possible, rather than deferring them.
- All code is **full-fledged**. Never use `t.Skip` or placeholder stubs to pass off **unfinished or unimplemented** work as "done". This does not forbid deliberate, sanctioned skips: the soak/nightly layer gates (`testlayers.RequireSoak`/`RequireNightly`, which skip when their layer is inactive) and genuine environment-precondition skips (for example, when an optional external tool is absent) are expected wherever the test-layer rules below call for them.
- When you encounter a pre-existing bug, fix small, clear, in-scope defects immediately and then resume the work you were doing. For a bug that is large, risky, or would materially widen the scope of the current task, follow the [Decision autonomy](#decision-autonomy) rule and ask the user how to proceed before acting.

### Regression prevention

Whenever you identify a bug, create the regression tests needed to guarantee that the same defect cannot reappear as a consequence of future development. A fix is not complete until a test that fails on the old behaviour and passes on the corrected behaviour is in place.

### Perfection Oriented

**Every action you take** — development, fixes, evaluations, analyses, audits, and any other — must be carried out to the standard of rigour demanded by a **production** environment, and carried out in an **exemplary and perfect** manner. Across the entire cycle — analysis → planning → development → testing — the result must be production-grade: apply your full knowledge and effort so that every change yields code ready to run in production, never a prototype or a partial solution.

The standard each component and the architecture must meet is set out in [Component and Architecture Excellence](#component-and-architecture-excellence).

### Never guess — evidence over assumption

Base every action **exclusively on verified knowledge**; never guess the intended answer.

- When the project **Knowledge Graph** holds the answer, consult it first (see [Knowledge Graph](#knowledge-graph)).
- When your knowledge is insufficient, research the answer in **official or authoritative sources** — specifications, peer-reviewed papers, books, or recognised authorities in the field — before deciding.
- When an open-source project already implements or solves the same problem, **read its source code** — the code, not its documentation, is what actually runs. See [Reference Projects (Open-Source Prior Art)](#reference-projects-open-source-prior-art).
- This generalises the openCypher conformance rule: never claim behaviour from memory.

### Measure to decide

Whenever you must assess **performance**, **completeness**, or **correctness**, gather evidence from the project and decide **empirically** — never by intuition. Benchmarks, test results, profiles, and Knowledge Graph queries are the basis for every such decision. This is the umbrella principle; the benchmarking mandates in [Performance-First Engineering](#performance-first-engineering) are its concrete, authoritative application to performance work.

### Decision framework — correct → secure → fast

When deciding what the project should deliver — in audits and evaluations as much as in implementation — apply this priority order:

1. **Is it correct?** Does the result meet the objective, the project specification, and the applicable authoritative sources (RFCs, standards, the openCypher specification, the ACID contract)?
2. **Is it secure?** Does the decision introduce nothing that compromises the safe use of the deliverable?
3. **Is it fast?** Is it as fast as it can be without compromising correctness or security, and what more can be done to maximise the deliverable's performance?

Correctness outranks security, and security outranks speed: never trade a higher priority for a lower one. This order governs **trade-off priority**, whereas [Performance-First Engineering](#performance-first-engineering) governs the **rigour** of the performance work itself, which is pursued only once correctness and security are assured. When these criteria conflict, or are hard to satisfy together, stop and ask the user how to proceed, presenting the available options.

---

## Token Economy

### Principle of action

**Before performing any operation, weigh its cost in tokens and choose the cheapest alternative that produces the same result.** When two or more routes lead to the same information — or to the same effect — the cheaper route is mandatory.

**Taking the cheap route must never affect the result of the operation.** The economy applies **exclusively to the means** used to reach the result, **never to the result itself**. What the cheap route returns must be **identical** to what the expensive route would have returned — not "close enough", not "approximately the same", not "probably the same": **identical**.

**Mandatory precondition — the equivalence test.** You may take the cheaper alternative only when you are certain the result is equivalent. Before choosing, verify:

- Does it return exactly the same information, with the same accuracy and the same level of detail?
- Does it cover exactly the same scope — the same files, the same cases, the same data?
- Does it produce exactly the same effect on the project?

If the answer to any of these is "no" or "I do not know", the cheap alternative is **excluded** and you use the route that guarantees the result. **Whenever equivalence is in doubt, take the more reliable route, however much more it costs.** Economy is only the tie-breaker between options proven to be equivalent — never a criterion for deciding the result itself.

**Never reduce, in order to save tokens:** the scope of the task, the depth of the analysis, the number of files or cases examined when all of them are relevant, the tests to run, the evidence to gather, verification against authoritative sources, the validation of acceptance criteria, or the quality of the deliverable. Saving tokens **is not** doing less: it is doing the same by a shorter route.

**Precedence.** Token economy **never** justifies compromising correctness, security, completeness, or the gathering of evidence. If the cheaper route yields a different, incomplete, or uncertain result, then it is **not the same operation** — and in that case [Never guess — evidence over assumption](#never-guess--evidence-over-assumption), [Measure to decide](#measure-to-decide), and the [Decision framework — correct → secure → fast](#decision-framework--correct--secure--fast) prevail. Saving tokens must never lead you to guess or to assume.

### Concrete applications

**Prefer the local CLI — the general rule.**

- **If an operation can be performed locally through a CLI, it must be performed through the CLI and by no other route.** The local CLI is systematically the cheapest option, so where an equivalent command exists, no other way of obtaining the same result is acceptable.
- This governs every more expensive alternative: web queries, browser tooling, navigating graphical interfaces, or any remote service that returns what a local command already returns.
- Examples:
  - `git log`, `git show`, `git diff`, `git blame` locally, instead of consulting the repository's web interface.
  - `gh issue view`, `gh pr view`, `gh api` (the GitHub CLI), instead of opening the corresponding web pages.
  - `rmp` for everything concerning tasks, sprints, and the Knowledge Graph (see [Planning and Task Execution](#planning-and-task-execution) and [Knowledge Graph](#knowledge-graph)) — which is in any case the single source of truth.
  - `--help`, `man`, or the command's own documentation, instead of searching for the same documentation online.
  - Filtering and aggregating data locally (for example with `grep`, `jq`, `sort`, `wc`), instead of pulling the full set into context.
- Reserve the more expensive routes — web, browser, remote services — for the cases where **no** local command can produce the same result.
- This preference is equally subject to the equivalence test above: if the CLI does not return the same information, with the same scope and accuracy, use the route that guarantees the result.

**Obtaining external information.**

- When a repository can be cloned — preferably `git clone --depth 1` — and its files read locally, **avoid** `WebFetch` for the same content, above all when several files from the same repository are needed.
- To consult a dependency's documentation, prefer what is already available locally: project files, the dependency's source, `go doc`, the command's `--help`. An internet search is the fallback, not the first move.
- When a web search is genuinely necessary, run **one targeted, specific search** rather than several generic searches followed by reading irrelevant pages.

**Consulting this project.**

- Consult the **Knowledge Graph first** (see [Knowledge Graph](#knowledge-graph)). Reading the graph is cheaper than reading files or walking the code for the same answer — this is precisely what the graph is for.
- Use targeted searches (`grep`/`glob` with precise patterns) instead of reading whole files to find a reference.
- When reading a large file, read only the range of lines needed rather than the whole file.
- For wide sweeps — many files or directories — **delegate to a sub-agent** that returns only the conclusion, instead of pulling the content of every file into the main context.

**Never repeat work already done.**

- Do not re-read files already read in this session, and do not re-confirm an edit that was applied successfully.
- Do not re-derive facts already established in the conversation, and do not reopen decisions the user has already taken.
- Do not launch the same search twice — for example, delegating a search to a sub-agent and also running it yourself. Delegate **or** run it, never both.

**Commands and output.**

- Limit command output to what is needed: `git log --oneline`, `git diff --stat` before the full diff, `git status --short`, `--name-only`, the `-q`/`--quiet` flags, or bound the result (for example with `head`).
- Avoid dumping large files into the context or into a reply. Reference `path:line` instead of reproducing the content.
- Prefer reading text — or a page's accessibility tree — over capturing images or screenshots, which are substantially more expensive, whenever the text suffices.

**Tests and validation.**

- **`make ci` runs ONCE, at SPRINT CLOSE — never per task.** The full gate takes
  roughly fifteen minutes and re-runs the entire module, so running it after every
  task spends hours re-proving what has not changed. It runs at the close of the
  sprint, and before any push.
- **Per task, run the targeted validation instead:** the package under change, its
  direct dependents, and any gate the change can plausibly move — plus the
  compliance gates the change touches (the openCypher TCK when `cypher/` changed,
  the crash/recovery battery when `store/` changed). Name in the task's closing
  record exactly what was run **and what was left unverified until sprint close**;
  never imply the full gate passed when it was not run.
- While iterating within a task, narrow further still: run the single test under
  change, not its whole package.
- **This relaxes no gate, it relocates one.** `make ci` — `go test -race ./...`,
  the TCK regression gate, `goleak`, and the lint pass — still runs in full, and
  every [Compliance Mandate](#compliance-mandates) and
  [Reliability and Concurrency Mandate](#reliability-and-concurrency-mandates)
  still has to be green before the sprint closes and before anything is pushed. What
  changes is the frequency, not the standard.
- **Read the exit status from inside the log, never from the wrapper.** A
  `make ci | tail` pipeline reports the exit code of `tail`: a real
  `make: *** [test-short] Error 1` has been masked as success this way. Redirect the
  whole run to a file, append the exit code to it, and read that.

**Model, effort, and parallelism.**

- Match the model and the reasoning-effort level to the real difficulty of each operation (see [Execution](#execution)): simple, mechanical operations do not justify the most expensive model or the highest effort.
- Group tool calls that are independent of one another into a single message, instead of issuing them one at a time. The sole exception is `git`: every `git` command runs alone, never grouped or chained with another command (see [Git command execution](#git-command-execution)).
- Respect the limit of two concurrent evaluations or audits (see [Execution](#execution)): excessive parallelism multiplies cost without accelerating the result.

### Safeguard

Every application above is subject to the equivalence test in [Principle of action](#principle-of-action). They are shortcuts on the **route**, never cuts in the **result**.

If, while executing, you find that the cheap route you chose is not producing the same result — it returned insufficient information, left part of the scope out, or raised doubt — **abandon it immediately and redo the operation by the complete route**. Tokens already spent are never a justification for accepting an inferior result.

---

## Planning and Task Execution

### Single source of truth

Use the `rmp` CLI (available system-wide) as the **sole source of truth** for all planning and task tracking in this project. No other tool or method should be used for this purpose. Drive every task- and sprint-related operation through the `roadmap-manager` skill, which is the sole operator of the `rmp` CLI.

The same `rmp` instance also hosts the project **Knowledge Graph** (see [Knowledge Graph](#knowledge-graph)) — the authoritative model of what the project *is* (its components, features, and provenance), distinct from rmp's role as the source of truth for *planning and tasks*. Consult it throughout planning to understand the components involved, their relationships, and the scope and impact of the proposed work.

### Planning

Before writing any code, analyse the proposed scope and determine whether multiple development phases are needed. Each phase must deliver a solid, standalone deliverable. Use the **Knowledge Graph** to map the affected components, their dependencies, and the blast radius of the change before committing to a plan.

**Phase/sprint definition (first pass):**
Define the phases (sprints in `rmp`) and the objective of each sprint before enumerating individual tasks.

**Task definition (second pass):**
For every task, document clearly:
- **Objective** — what it accomplishes.
- **Functional requirements** — observable behaviour expected.
- **Technical requirements** — constraints, interfaces, performance targets.
- **Acceptance criteria** — the concrete, verifiable conditions that must be met to close the task.

When the work spans multiple sprints, complete the full sprint list first, then populate tasks sprint by sprint.

Use the **Knowledge Graph** to identify the **foundational and highest-leverage tasks** — those that unblock the most downstream work or carry the widest impact — and sequence the plan to tackle them first.

**Prioritisation.** By default, always work from the highest-gain, highest-impact tasks down to the least essential. Foundational tasks, and tasks that unblock other tasks or features, are always tackled first.

**Task sizing.** When a task is too large to be completed in a single pass by an AI agent, subdivide it into parts, each of which still honours the self-contained-development principle.

### Execution

Task execution is the natural continuation of planning. For each unit of work, follow this sequence using `rmp`:

1. Check whether any open task is already in progress and, if so, continue it.
2. Identify the next task to start.
3. Read and fully understand the task — its objective, functional and technical requirements, and acceptance criteria — consulting the **Knowledge Graph** to gauge its scope and impact.
4. Determine the most appropriate sub-agent for the work and delegate its execution to that specialist, under the rule in [Every task is developed under a specialist sub-agent](#every-task-is-developed-under-a-specialist-sub-agent).
5. Implement the task, then verify that **all** acceptance criteria are satisfied before considering it done.
6. Close the task with a concise summary of what was done.
7. Create a **git commit** following conventional-commit conventions and describing what was done, before moving to the next task.
8. Update the **Knowledge Graph** to reflect the change (see [Knowledge Graph](#knowledge-graph)), stamping the affected nodes and edges with the commit hash and date.

**Sequencing rules:**
- **Task and sprint execution is strictly sequential.** Sprints run one at a time, and within a sprint tasks run one at a time. There is no justified exception: do not overlap two tasks, however independent they appear.
- **Evaluations and audits** are the sole exception. They may run in parallel, but any such parallel execution **always requires the user's explicit prior authorisation** (see [Sub-Agents (Specialists)](#sub-agents-specialists)).
- **Never run more than two (2) evaluations or audits at once**, even once authorised. Plan the full set up front, then execute them at most two at a time, starting the next only as one finishes, so the limit of two concurrent is never exceeded.

**Model and effort.** Wherever possible, match the model and its reasoning-effort level to the demands of each individual operation within a task.

---

## Knowledge Graph

Maintain a project **Knowledge Graph (KG)** using the graph features of `rmp` (its built-in *Groadmap* graph), driven through the `knowledge-authority` skill — the empirical single source of truth about the project's own contents, which queries the graph store first and reads source files only as a fallback. The KG is the authoritative, queryable model of the project, and you must keep it as current as possible. To **locate and understand project structure** — components, features, tests, and provenance — query the graph first and fall back to reading source files only when the graph cannot answer. This is a navigation shortcut, not authority over the code: for any question of *actual behaviour* — above all openCypher conformance — the primary sources still govern, so consult the specification, the relevant TCK feature files, and the source itself as the [Compliance Mandates](#compliance-mandates) require.

### What the graph must capture

The Knowledge Graph **must hold everything useful to know about the project**, including:

- **Features** — what they are, where they are specified, and where they are implemented.
- **Tests** — which tests exist and what each one verifies.
- **Components** — the building blocks of the module, how they relate, and the dependencies between them.
- **Provenance** — the git commit in which each feature was specified, the commit in which it was implemented, and the commit in which it was tested.
- **Planning** — the `rmp` sprints and tasks, and how they map onto components and features.

Create whatever node and edge types best serve the project and your work; use the graph together with sprints and tasks to coordinate development.

### Keeping the graph current

- **Update the graph at every git commit.** Record the change on the affected nodes and edges, and stamp each with the **commit hash and date**.
- Treat the graph as the **authoritative model of the project**: when it contradicts your *assumptions*, trust the verified graph over memory. The code itself remains the ground truth for what is *actually implemented* — when the graph and the code disagree, the code wins and you reconcile the graph to it.
- As you discover new relationships while working, store them in the graph so the project's knowledge compounds over time.

---

## Component and Architecture Excellence

### Exemplary components

Every component of the project must be an **exemplary** piece of engineering for the purpose it serves. "It works" is not the standard; being an exemplary implementation of its kind is.

- **Explicit responsibility.** Every component states its responsibility clearly and explicitly, so that its boundaries of action are unambiguous. What it owns, what it does not own, and where its responsibility ends must be written down — in its package documentation and in the [Knowledge Graph](#knowledge-graph) — never left to be inferred from the code.
- **Boundaries are enforced, not merely described.** A component does not reach across its boundary: it collaborates through the interfaces its neighbours expose. A responsibility that has leaked across a boundary is a defect, to be fixed like any other.
- **Designed, implemented, and evaluated against prior art.** To **design**, **implement**, and **evaluate** each component, research which open-source projects implement that functionality in an exemplary way, and take those implementations as a source of inspiration by following [The inspiration protocol](#the-inspiration-protocol). More than one reference project may — and normally should — inform the same component.

### Architecture

The project's overall architecture, and the specific architecture of each of its components, must rest on the best practices that best fit the project's purpose — not on practice that is merely conventional, familiar, or fashionable.

- **Fit for purpose first.** A pattern is adopted because it serves GoGraph's purpose: a small, ergonomic API; fast search; ACID durability; and reliability under high load and high concurrency. A pattern that does not serve those ends is rejected, however well regarded it is elsewhere.
- **Informed by prior art.** Seek inspiration in other open-source projects so that the intended results are reached deliberately and assertively, following [The inspiration protocol](#the-inspiration-protocol) and the references in [Reference Projects (Open-Source Prior Art)](#reference-projects-open-source-prior-art).
- **Ranked by the decision framework.** Architectural choices are ranked by [correct → secure → fast](#decision-framework--correct--secure--fast) and, when they bear on performance, held to [Performance-First Engineering](#performance-first-engineering).
- **Recorded.** The architecture in force is documented in [Intended Architecture](#intended-architecture) and modelled in the [Knowledge Graph](#knowledge-graph). Changing it is a change of architecture, and therefore requires the user's prior agreement under [Decision autonomy](#decision-autonomy).

---

## Reference Projects (Open-Source Prior Art)

GoGraph is not built in a vacuum. For every feature the module implements, **use as reference and inspiration every open-source project that implements or solves the same — or a similar — problem**. Those projects are the empirical record of what the open-source community has learned about the technical and architectural problems GoGraph faces, and they must inform its decisions.

**The source code is the ultimate source of truth.** Documentation, blog posts, and papers state intent; the code is what actually runs. Whenever it matters — and it usually does — read the reference project's source to verify how the approach is really implemented, what the real data structures and code paths are, and what the trade-offs actually cost. Then assess what adopting that approach would mean for GoGraph.

**Use more than one reference per component.** Wherever it is possible, study at least two projects that solve the same problem, so that the strengths **and** the weaknesses of each approach can be compared rather than inherited. One reference tells you what a single team chose; two or three tell you which parts of that choice are essential and which are incidental to their context.

### Primary references

| Project | Reference for |
|---|---|
| **Neo4j** | The canonical LPG graph database: Cypher planner and semantics, store record format, transaction and lock management, index and constraint machinery, Bolt protocol. |
| **Memgraph** | In-memory-first LPG graph database: in-memory graph representation, MVCC deltas, query execution, snapshots and WAL. |
| **ClickHouse** | Columnar storage and execution: vectorised block-at-a-time processing, compression and encoding schemes, late materialisation, aggregation. |
| **DuckDB** | Embedded columnar analytics engine: vectorised push-based execution, morsel-driven parallelism, columnar storage, optimiser design. Closest to GoGraph in embedding model — a library, not a server. |
| **PostgreSQL** | The reference relational engine: WAL design and crash recovery, MVCC and visibility rules, buffer management, cost-based planner and statistics, B-tree implementation, isolation levels. |
| **MariaDB** | Widely deployed relational engine: storage-engine abstraction, redo/undo logging, group commit, durability settings. |

**Neo4j and Memgraph carry particular weight.** They implement the same solution as GoGraph — a Label-Property-Graph database — in both architecture and purpose, so on any question of graph-database behaviour, architecture, or performance, look there first. **ClickHouse and DuckDB** are the references for the columnar component, and **PostgreSQL and MariaDB** for the mechanics of databases proven at scale by a very large community.

This does not displace the [Compliance Mandates](#compliance-mandates): openCypher conformance is governed by the specification and the TCK, never by how one implementation happens to behave. Where a reference implementation diverges from the specification, the specification wins.

### Beyond the primary list

The table above is a floor, not a ceiling. **Research and use any other open-source project** that solves a technical problem GoGraph also has, subject to two conditions:

1. It implements or solves a feature that actually exists in GoGraph.
2. It is open source, so its implementation details can be read and **verified empirically in the code** — never taken on faith from documentation.

Reach for whatever fits the problem at hand: for example RocksDB, LevelDB, or SQLite for durable storage and recovery mechanics; Apache Arrow, Parquet, or Velox for columnar layout and encoding; Kùzu, FalkorDB, or Apache AGE for alternative graph-engine designs; the Go standard library and well-regarded Go libraries for idiom and allocation behaviour. Choose by problem, not by fame.

### How prior art feeds decisions

- **It serves four quality axes.** Every insight harvested must make GoGraph a more exemplary implementation in **Performance**, **Efficiency**, **Correctness**, and **Security**. An insight that serves none of these is noise.
- **It makes decisions objective and assertive.** A design choice backed by how two or three mature engines actually solved the same problem is a settled question; an unbacked preference is not. Use prior art to close decisions, not to widen them.
- **It is evidence, not authority.** The [Decision framework — correct → secure → fast](#decision-framework--correct--secure--fast) still ranks the trade-offs, and [Measure to decide](#measure-to-decide) still requires that any claimed win be measured **in GoGraph itself**: a technique that is fast in C++ or on the JVM may lose in Go. Benchmark before adopting, and record the result.
- **Extract the insight, not the code.** Take the structural idea — the algorithm, the memory layout, the ordering of operations — and re-implement it idiomatically in Go. Copying source from a reference project into GoGraph is forbidden; see [Never copy — reimplement](#never-copy--reimplement).
- **Cite what you consulted.** When a reference project influences a non-obvious decision, record which project, which version or commit, and which file or component you read — in the task description, a code comment, or the audit document — exactly as [Sub-Agents (Specialists)](#sub-agents-specialists) requires of specialist findings.
- **Store what outlives the task.** Comparisons and insights of lasting value belong in the [Knowledge Graph](#knowledge-graph), so the project's understanding of the prior art compounds instead of being re-derived each cycle.

### The inspiration protocol

Before designing or implementing any component, **state clearly and objectively what that component is for**. Only then — and always as a function of that objective, the macro objective first — study how the leading or most successful open-source projects solved the same problem, and use that knowledge to take better-informed decisions **for this project**.

Reference projects are treated as **good practice to be analysed**, never as a solution to be adopted automatically. What is extracted from them is **understanding** — the structure, the algorithm, the reason for the decision, the trade-offs accepted — never code to transcribe.

Follow this sequence for each component:

1. **Define the component's macro objective.** What problem it solves, what role it plays in the project, what guarantees it must offer, and under which constraints (correctness, security, performance, durability, concurrency). Written explicitly and without ambiguity.
2. **Define the micro objectives.** The concrete features and behaviours: inputs and outputs, invariants, edge cases, quality and performance requirements, and acceptance criteria (see [Planning](#planning)).
3. **Record the objectives and the decisions in the Knowledge Graph** (see [Knowledge Graph](#knowledge-graph)), so that they remain queryable and traceable.
4. **Identify the reference projects.** Select the open-source projects that solve the same class of problem with recognised success. Selection criteria: maturity and real adoption, active maintenance, demonstrable engineering quality, documented design, and production use — **not** popularity on its own. The identification must be verified, never presumed (see [Never guess — evidence over assumption](#never-guess--evidence-over-assumption)).
5. **Study the approach in primary sources.** Source code at a concrete version or tag, official documentation, design documents, ADRs, papers, and issue or pull-request discussions — in preference to secondary sources. The aim is to understand **why** the decision was taken, not merely what it was. To study a repository, apply [Token Economy](#token-economy): clone it locally instead of issuing many remote requests.
6. **Analyse the favourable AND the unfavourable aspects.** For each approach, enumerate explicitly:
   - what serves this component's objective, and why;
   - what does **not** serve it, and what problems it would cause here;
   - which trade-offs the approach accepts;
   - what the reference project's premises and context were (scale, language, concurrency model, durability requirements, runtime environment), and **whether those premises hold in this project**;
   - what the reference project **abandoned** over time, and why — negative evidence is frequently the most valuable of all.
7. **Decide for this project.** The decision follows from the objectives defined in steps 1 and 2 and from the [Decision framework — correct → secure → fast](#decision-framework--correct--secure--fast). It is expected to be an **adaptation or a synthesis**: it may combine ideas from several references, or reject them all, provided it is reasoned.
8. **Document the decision.** Record the decision taken, the alternatives considered, the sources consulted, and the reasoning, in a form that can be audited and revisited.
9. **Validate empirically.** When the approach has a measurable impact, measure it **in this project** rather than trusting the reference's claims (see [Measure to decide](#measure-to-decide)).

### Never copy — reimplement

- **Copying code directly from open-source projects into GoGraph is forbidden**: whole files, blocks of code, or line-by-line transcription or translation into another language.
- The implementation must be **original**, idiomatic for Go and for this project's conventions, and designed for the objectives defined in [The inspiration protocol](#the-inspiration-protocol).
- **Copying a decision without understanding it is equally forbidden.** Adopting an approach merely because a reference project uses it is a form of guessing (see [Never guess — evidence over assumption](#never-guess--evidence-over-assumption)). If you cannot explain why it suits this component, do not adopt it.
- **Licences and legal obligations.** Inspiration does not dispense with respecting the source project's licence. Several primary references are copyleft or source-available — Neo4j is GPLv3, MariaDB GPLv2, Memgraph BSL 1.1 — and none of their licences is GoGraph's to redistribute. Never incorporate third-party code without checking the licence **and without the user's explicit authorisation**. If you conclude that reusing code or adopting a dependency is the best route, **ask the user first** (see [Decision autonomy](#decision-autonomy)), presenting the options and identifying the licence of each.
- **Attribution.** Record in the [Knowledge Graph](#knowledge-graph) and in the documentation which source inspired each decision — for traceability and credit, never as a way of legitimising a copy.

### Prior-art safeguards

- **"That is how project X does it" is never, on its own, a justification.** The justification is always this component's objective. Popularity is not fitness.
- **A different context invalidates conclusions.** Compare the premises before comparing the solutions: an approach that is excellent in its own context may be unfit here.
- **Approaches evolve.** Study a concrete version, and verify whether the approach is still in force in the reference project.
- When a reference approach conflicts with this project's specification or objectives, **ask the user** how to proceed (see [Decision autonomy](#decision-autonomy)), presenting the options.

---

## Performance-First Engineering

### Research methodology before any implementation

Before writing a single line of code for any non-trivial component, conduct a **cross-language, cross-paradigm survey** of every known approach. This means:

1. **Survey the academic and engineering literature, and the open-source prior art** — consider how the problem is solved in C, C++, Rust, Java (JVM JIT tricks), Python (CPython/PyPy), and specialised graph databases (Neo4j, DGraph, JanusGraph, TigerGraph). Read the source of the projects listed in [Reference Projects (Open-Source Prior Art)](#reference-projects-open-source-prior-art) that solve the same problem. Extract the structural insight, not the syntax.
2. **Identify the performance ceiling** — determine what the theoretically optimal time and space complexity is for the problem, and whether any real-world implementation reaches it.
3. **Evaluate data structure alternatives** — for every hot-path structure, explicitly compare at least two candidates (e.g., adjacency matrix vs. CSR vs. adjacency list; binary heap vs. Fibonacci heap vs. pairing heap for priority queues) with measured or cited trade-offs.
4. **Translate to idiomatic Go** — implement the winning approach using Go idioms: no `interface{}` in hot paths, avoid unnecessary heap allocations, favour value semantics for small structs, use `unsafe` only when justified and documented.

### Go-specific performance mandates

- **Prefer flat, cache-friendly data structures** — a `[]Edge` slice with index arithmetic beats a `map[NodeID][]Edge` in cache-miss-sensitive traversal.
- **Avoid interface dispatch in inner loops** — use concrete types internally; expose interfaces at package boundaries only.
- **Pre-size all slices and maps** — always pass a capacity hint when the upper bound is knowable.
- **Use `sync.Pool` for ephemeral allocations** — priority queues, visited sets, and path buffers that are created per query must be pooled.
- **Benchmark before and after every structural change** — use `go test -bench=. -benchmem -count=5` and compare with `benchstat`. A change that regresses allocations/op or ns/op without a documented justification must not be merged.
- **Profile with `pprof`** — CPU and heap profiles must be checked for any algorithm operating on graphs with more than 10k nodes.

### Idiomatic Go requirements

- **Error handling** — return `(T, error)`; never panic on recoverable conditions; never swallow errors.
- **Generics** — use type parameters (`[N comparable, W constraints.Ordered]`) for node IDs and edge weights so the library is not tied to `int64`/`float64`.
- **Concurrency** — prefer channels for coordination between goroutines; use `sync.RWMutex` for shared graph state; document every exported type's concurrency contract.
- **Package naming** — single-word, lowercase, no underscores; package names must not stutter with their exported identifiers (`graph.Graph` is acceptable; `graph.GraphGraph` is not).
- **Tests** — table-driven tests with `t.Run`; property-based tests with `testing/quick` or `pgregory.net/rapid` for algorithms where invariants can be expressed generically.
- **Test layers** — every test belongs to one of three layers:
  - `short` — the default; runs on `go test ./...` with no tags. Run the packages a change touches on every change; the whole layer runs via `make ci` at sprint close and before every push (see [Tests and validation](#concrete-applications)). The per-package cost budget, the ceilings that are enforced, and the measured exceptions are specified in [`docs/test-layers.md`](docs/test-layers.md) and are deliberately **not** restated here: this file carried its own copy of the number and the two drifted apart.
  - `soak` — minutes-long workloads. Activated by the `soak` build tag or by setting `SOAK_FULL=1`. The pre-existing `stress` and `soakfull` build tags are considered part of the soak family.
  - `nightly` — hours-long workloads. Activated by the `nightly` build tag or by setting `GOGRAPH_NIGHTLY=1`; implies soak.

  Prefer compile-time gating with a `//go:build soak` or `//go:build nightly` header on a dedicated file; when that is impractical, call `github.com/FlavioCFOliveira/GoGraph/internal/testlayers.RequireSoak(t)` or `RequireNightly(t)` at the top of the test body. The full specification, including sample invocations and the helpers' API, lives in [`docs/test-layers.md`](docs/test-layers.md).

  The production-readiness test battery — shape generators, invariant checkers, fault-injection packages, dataset loaders, and the add-new-shape recipe — is documented in [`docs/test-battery.md`](docs/test-battery.md).

---

## Reliability and Concurrency Mandates

This module must operate **without failure under sustained high load and high concurrency**. Every component — public or internal — must satisfy the following non-negotiable contract.

### Correctness under concurrency

- **Zero data races.** `go test -race ./...` must pass on every change. No exceptions. Run it locally via `make ci` before every push; a change with a reported race must not be merged.
- **Explicit concurrency contract.** Every exported type carries a godoc clause stating whether it is safe for concurrent use, and if so under which operations. Ambiguity is a defect.
- **No hidden global state.** Package-level mutable variables are forbidden outside of carefully reviewed registries; every shared resource is passed explicitly.
- **Context-aware blocking.** Every public API that may block on I/O, a channel, a lock, or a long computation accepts a `context.Context` and honours cancellation and deadlines.

### Robustness under load

- **Bounded resources.** No unbounded queues, no unbounded goroutine spawn, no unbounded caches. Every queue, pool, semaphore, and cache declares an explicit upper bound surfaced in its constructor.
- **Backpressure, never panic.** When a downstream is saturated, callers either receive a typed error or block on a bounded channel; the library must never crash, deadlock, or silently drop work.
- **No goroutine leaks.** Every goroutine spawned by the library has a defined lifecycle and is terminated on `Close`/`Shutdown`/context cancellation. Verified via `go.uber.org/goleak` in test teardown for every package that spawns goroutines.
- **Graceful degradation.** Under memory pressure or saturation, the library degrades predictably (slower, fewer concurrent operations) rather than failing catastrophically.

### Performance under contention

- **Lock contention budget.** Hot paths must not hold a global lock. Use sharded structures (`hash(NodeID) mod N` shards), lock-free atomics, or copy-on-write snapshots. Any new global mutex requires explicit justification in code review.
- **Readers do not block writers and vice versa where avoidable.** Prefer `atomic.Pointer[Snapshot]` swap patterns for read-heavy workloads; use RW-mutexes only when a fully lock-free design is impractical.
- **Lock-free read paths on immutable structures.** Traversal of an immutable CSR snapshot must require zero synchronisation primitives in the hot loop.
- **Fair scheduling.** Long-running operations yield (`runtime.Gosched` or chunked work) to keep latency tails bounded for concurrent short queries.

### Failure handling

- **Fail-stop, never fail-silent.** Errors are returned, logged, and counted via metrics. Corrupted state triggers a clean shutdown via a sentinel error rather than continuing with silent inconsistency.
- **Defensive validation at boundaries only.** Internal code trusts its callers; external inputs are validated once at the public API boundary and never again.
- **Recoverable panics are forbidden.** The library does not `recover()` to hide bugs; panics indicate programmer error and must surface immediately. Exception: goroutines that the library owns may recover **only** to log the panic, record it as a metric, and terminate cleanly.
- **Crash safety.** Any persistent state survives `kill -9` mid-write. Verified by deterministic crash-injection tests (Sprint 3 acceptance criterion).

### Observability

- **Every long-lived goroutine is observable.** Name (via `pprof.SetGoroutineLabels`), lifecycle metrics (started, running, exited), and recent activity timestamp.
- **Every cache, pool, and bounded queue exports utilisation metrics.** Size, capacity, hit ratio, eviction count, blocked-acquire count.
- **Latency histograms on every public blocking API.** Prometheus exposition format, with consistent label names across the module.
- **Race-detector and `goleak` integration in the local gate.** Both run under `make ci` (`go test -race ./...`); both must be green before a change is pushed or merged.

### Acceptance gates

- **Soak test (periodic reliability exercise; not a release gate).** A multi-hour mixed-workload run under `GODEBUG=gctrace=1` should show zero growth in heap, file descriptors, and goroutine count after warm-up. Run it periodically — and ideally before a major release — but it does **not** block a release.
- **Concurrency stress test in the local gate.** A short variant of the soak workload runs as part of the race-enabled short test layer (`make ci`) before every push.
- **Load-test report alongside benchmarks.** Each release ships latency and throughput numbers at multiple concurrency levels (1, 8, 64, 256, 1024 goroutines), recorded in `docs/benchmarks/`.

---

## Sub-Agents (Specialists)

Your working team comprises **all available sub-agents** — global, user, and project. Use them collaboratively and complementarily so that every task is completed with the greatest possible confidence, effectiveness, and assertiveness, with each specialist contributing its expertise proactively. The specialists in the table below **must be actively consulted** to maximise output quality — do not implement a component in isolation when a relevant specialist can provide material input — but treat the table as the key specialists, not the limit of the team.

| Agent | When to invoke |
|---|---|
| `graph-theory-expert` | Graph model selection, algorithm correctness, complexity analysis, data structure trade-offs for graphs specifically. Consult **before** choosing any algorithm or graph representation. |
| `go-developer` | Go implementation, idiomatic patterns, module structure, Go toolchain. The primary implementation agent. |
| `rust-elite-developer` | Cross-language performance insight: zero-copy patterns, arena allocation, SIMD, lock-free structures. Translate findings to Go. |
| `rust-perf-engineer` | Hot-path profiling methodology, cache behaviour, concurrency bottleneck diagnosis. Apply findings to Go benchmarks. |
| `Plan` | Architectural decisions before any sprint begins. Use for evaluating alternative designs when the stakes are high. |

### Every task is developed under a specialist sub-agent

**Every task is developed under the isolation of a sub-agent, and never by the
coordinating agent directly.** This is not a preference about who types: it keeps
each task's exploration, dead ends, and file churn out of the coordinating
context, so the coordinator retains the judgement to verify the result instead of
having spent itself producing it.

**Choosing the specialist.** Pick the available sub-agent whose expertise matches
the work the task actually consists of. That match is read from **two** sources,
and both are mandatory:

- **The task** — its title, its functional, technical and acceptance criteria,
  and above all its **comment log**. The comments are the brief; the fields alone
  are not. A task whose fields describe a version upgrade while its comments
  record that the upgrade already happened and the decision was reversed will
  send the wrong specialist after the wrong problem.
- **The sprint the task sits in** — its title, its description, and its comment
  log. The sprint states the macro objective the task serves, and a task
  executed outside that objective is executed wrongly even when its own criteria
  are met.

Read both before choosing, and pass both to the specialist as context. A
specialist that does not know the sprint's purpose cannot tell which of several
correct-looking approaches serves it.

**What the coordinator still owns**, and must not delegate:

- reading the task and the sprint, and writing the specialist's brief;
- **verifying the specialist's claims against independent evidence** — a
  specialist's report is a finding to check, not a result to relay;
- the git commits, the `rmp` state transitions, and the task's closing record;
- every decision reserved to the user by [Decision autonomy](#decision-autonomy).

**Boundaries.** One task at a time, as [Execution](#execution) requires: several
specialists may run concurrently only when they serve the *same* task with
independent inputs, or when the user has authorised parallel evaluations subject
to the cap of two. When two specialists could touch the same files, say so in
each brief and scope them apart; concurrent edits to one file by two agents
produce a result neither of them validated.

### Mandatory consultation rules

- **`graph-theory-expert` must be consulted** before finalising the representation of any graph type and before selecting any search or traversal algorithm.
- **`go-developer` must validate** all Go code for idiom conformance before a task is closed.
- Specialists may be **consulted in parallel** — to inform in-flight implementation — when their inputs are independent (e.g., consulting `graph-theory-expert` on algorithm choice while `go-developer` drafts an adjacent module).
- **Evaluations and audits run in parallel only with the user's explicit prior authorisation, and never more than two at a time.** Parallel *consultation* that informs implementation is always allowed when inputs are independent; running standalone *evaluations or audits* concurrently is not — it must be authorised by the user beforehand, and the concurrency cap of **two** applies even after authorisation. Plan every evaluation or audit the work needs, then run them two at a time, starting the next only as one finishes.
- Findings from specialists must be summarised in the task description or in a code comment when they influence a non-obvious design decision.

---

## Common Commands

```bash
# Initialise the module (first time only)
go mod init github.com/FlavioCFOliveira/GoGraph

# Build
go build ./...

# Test all packages (short layer only — the default local run)
go test ./...

# Test all packages — short + soak
go test -tags=soak ./...

# Test all packages — short + soak + nightly
go test -tags=nightly ./...

# Test a single package
go test ./graph/...

# Run a specific test
go test -run TestBFS ./graph/...

# Race detector (always use for concurrent code)
go test -race ./...

# Benchmark
go test -bench=. -benchmem ./...

# Lint (requires golangci-lint)
golangci-lint run ./...

# Format
gofmt -w .
goimports -w .

# Vet
go vet ./...
```

---

## Intended Architecture

The module is organised around three concerns:

| Layer | Responsibility |
|---|---|
| `graph/` | Core types: `Graph`, `Node`, `Edge`, `Weight`. Directed and undirected variants. |
| `search/` | Traversal and path-finding algorithms: BFS, DFS, Dijkstra, A\*, Bellman-Ford. |
| `store/` | Durable, WAL-backed persistence: `store.DB` composed with a write-ahead log, checkpointer, snapshot writer, and crash-recovery replayer (`store/{wal,checkpoint,snapshot,recovery,txn}`). The in-memory engine is `graph/lpg`; `store.DB` adds durability on top of it. |

### Key design rules

- **Interfaces over concrete types** — callers depend on `graph.Graph`, not on an adjacency-list struct, so backends and algorithms remain swappable.
- **Zero-allocation hot paths** — search algorithms must avoid heap allocations in their inner loops; use pre-allocated slices and `sync.Pool` where needed.
- **No global state** — every `Graph` instance is self-contained; concurrent read access must be safe without external locking.
- **Persistence is WAL-backed and crash-safe** — the durable database is `store.DB` (`store/db.go`), a composition of a write-ahead log (`store/wal`), a checkpointer (`store/checkpoint`), a snapshot writer (`store/snapshot`), a crash-recovery replayer (`store/recovery`), and the transaction layer (`store/txn`). There is no single `store.Store` interface and no `store/file` or `store/postgres` adapter: persistence is this fixed composition, not a pluggable backend registry. The in-memory engine (`graph/lpg`) runs without any of it; `store.DB` layers durability on top.

### Algorithm conventions

- Each algorithm lives in its own file inside `search/` (e.g., `search/dijkstra.go`).
- Algorithms accept a `graph.Graph` interface and return a typed result struct (path, distance map, etc.) — never raw `interface{}` or `any`.
- Provide both a simple one-shot function (`ShortestPath(g, src, dst)`) and a stateful struct for repeated queries on the same graph.

---

## Examples

Examples are **not an integral part of the GoGraph module** — the module neither imports nor depends on them. They are **instruments** — **exercise harnesses** and **usage simulators**: their sole role is to exercise GoGraph's features, both individually and in combination, under realistic conditions so that the module's behaviour — both its **correctness** (the assertiveness of its results) and its **performance** (its efficiency in the use of CPU, RAM, and storage) — can be observed and measured. Exercising an example must allow **measurements or observations** to be extracted for **every relevant indicator**, so that the module's **performance, behaviour, and efficiency** can be analysed **completely and objectively**.

### Organisation

- All demonstrative examples live under a single `examples/` folder at the **root** of the project.
- Each example is contained in its **own dedicated sub-folder** under `examples/`, used exclusively for that one example. No example shares a sub-folder with another.
- Each example sub-folder **must** contain a `README.md` that describes, in detail, the example's **scenario**, **objective**, and **purpose** — what real-world situation it models and what it sets out to demonstrate and measure.

### Mandate

Every example must **always** be realistic and a **faithful end-to-end demonstration** of using GoGraph. An example is never a throwaway toy: it is a real simulation of a scenario that, when run, lets us observe GoGraph's behaviour and understand its performance **objectively and empirically**.

Every example serves **three equally important objectives**:

1. **Demonstration** — a didactic, end-to-end illustration of how GoGraph can be used for a given scenario or purpose.
2. **Exercise** — drive the GoGraph features most appropriate to the example's scenario and overall purpose. Exercise the module not only through its most basic features but also through its most advanced ones, including the **combination of multiple features** working together.
3. **Evidence** — enable the objective and explicit collection of evidence while the features are exercised, so that GoGraph can be evaluated clearly along two complementary axes: its **correctness** — the assertiveness of the results it produces — and its **performance** across **all** resource vectors: memory (RAM), CPU, and storage.

### Evidence and tooling

Assess performance empirically, never by intuition: every claim about CPU, RAM, or storage must rest on collected data — the [Measure to decide](#measure-to-decide) principle applied to examples. Exercising an example must yield **measurements or observations** for **every relevant indicator** so that the module's **performance, behaviour, and efficiency** can be analysed **completely and objectively**. Use the tools best suited to the Go technology stack — and any other tool that yields pertinent information — to observe each behaviour in detail and to draw conclusions strictly from that data:

- **CPU and heap profiling** — capture `pprof` profiles (`runtime/pprof`, `net/http/pprof`) to attribute CPU time and allocations to specific call sites, and render them as **flame graphs** (`go tool pprof -http=:0`, or an exported SVG) to read the hot call stacks at a glance.
- **Execution tracing** — capture a `runtime/trace` profile and inspect it with `go tool trace` to observe scheduling, goroutine blocking, GC pauses, and syscall latency over the workload's timeline.
- **Allocations and latency** — drive the exercised paths under `go test -bench=. -benchmem` and compare runs with `benchstat`.
- **Live memory and GC** — sample `runtime.MemStats`, and run under `GODEBUG=gctrace=1` where GC behaviour is relevant, to track resident heap and its growth.
- **Code coverage** — measure which GoGraph code paths an example actually drives: build it with `go build -cover` and collect counters via `GOCOVERDIR`, then report with `go tool covdata percent`/`textfmt` (or `go test -coverprofile` for test-driven paths) and inspect with `go tool cover -html`/`-func`. Coverage reveals what the example does and does not exercise, guiding it towards the untested surface.
- **Storage footprint** — measure the on-disk size of the store directory and how it grows across the workload.

Each example surfaces its measurements as explicit telemetry so the evidence can be inspected and compared run to run.

Because the examples exercise the module under realistic conditions, treat them as a primary means to evaluate GoGraph and to identify **all** opportunities to improve its use of CPU, RAM, and storage. Feed every insight that emerges back into the project (Knowledge Graph, benchmarks under `docs/benchmarks/`, and the `rmp` backlog) so the examples continuously inform the module's evolution.
