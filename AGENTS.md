# Agent GitHub Rules

This file defines the GitHub rules agents must follow in this repository.

---

## 1. Repository-specific branch model

This repository uses a simple buffered branch model:

- `main`: stable branch.
- `dev`: integration branch for ongoing work.
- short-lived work branches: feature/fix/docs/test/refactor branches created from `dev`.

Rules:

- Do not work directly on `main`.
- Do not work directly on long-lived `dev` unless explicitly asked.
- Prefer short-lived branches from `dev` for implementation work.
- New short-lived branches must be created from the latest `origin/dev`, not from a stale local `dev`.
- Do not create a long-lived `develop` branch.
- Do not keep long-lived `release/*` branches.

Recommended flow:

```text
short-lived branch -> dev -> main
```

For the current early-stage repository, documentation-only changes may be made on `dev` when explicitly requested, but code implementation work should still use short-lived branches.

---

## 2. Short-lived work branches

Create short-lived work branches from the latest `origin/dev` using this format:

```text
<type>/<scope>/<short-kebab-description>
```

Required freshness flow before creating a branch:

```text
git fetch origin dev
git switch -c <type>/<scope>/<short-kebab-description> origin/dev
```

Do not branch from a stale local `dev`. If the branch already exists and `origin/dev` advanced, rebase the short-lived branch onto the latest `origin/dev` before opening or updating the PR.

Allowed `type` values:

- `feat`: new feature.
- `fix`: bug fix.
- `refactor`: behavior-preserving refactor.
- `chore`: build, dependency, repository, or process maintenance.
- `docs`: documentation.
- `test`: tests.
- `hotfix`: urgent fix.
- `release`: release preparation.

Recommended `scope` values for this repository:

- `docs`
- `architecture`
- `agent-runtime`
- `task-graph`
- `ctxlib`
- `workspace`
- `scheduler`
- `event-store`
- `ui`
- `config`
- `repo`

Examples:

```text
docs/architecture/split-module-docs
feat/agent-runtime/cli-discovery
feat/task-graph/state-machine
fix/ctxlib/context-pack-ranking
chore/repo/github-rules
```

Delete short-lived source branches after merge.

---

## 3. PR flow

Normal feature, fix, refactor, docs, and test changes flow as:

```text
short-lived branch -> dev -> main
```

Rules:

- Open implementation PRs into `dev`.
- Open release/promote PRs from `dev` into `main` when `dev` is ready to become stable.
- Do not open normal feature branches directly into `main`.
- Use direct hotfix-to-`main` only for urgent fixes, and then forward-merge or cherry-pick the fix back to `dev`.

---

## 4. Merge policy

- Short-lived branches into `dev`: use squash merge by default.
- `dev` into `main`: use merge commit so the integration boundary is preserved.
- Hotfix branches into `main`: use squash or merge commit according to repository maintainer preference, then back-port to `dev`.
- Never rebase permanent branches (`main` or `dev`).
- Short-lived branches may be rebased onto the target branch to resolve conflicts.

Release/promote PR titles from `dev` to `main` should use:

```text
release: dev to main vYYYY.MM.DD.N
```

If the project is not using versions yet, use:

```text
chore(release): promote dev to main
```

---

## 5. PR title and description

Use Conventional Commit style PR titles, for example:

```text
docs(architecture): split module detail docs
feat(agent-runtime): add cli discovery design
fix(ctxlib): clarify runtime retrieval contract
chore(repo): add agent github rules
```

Every manual PR description must include:

```text
## Issues
- Closes #<issue> or Refs #<issue>
- State whether this PR is a full fix, partial work, follow-up, promote, or release

## Summary
- What changed
- Why it changed

## Test
- Commands run and manual validation performed

## Risk
- Affected modules/docs
- Configuration, deployment, data, compatibility, or workflow risks
```

Use `Closes` only for the source implementation PR that fully satisfies the issue acceptance criteria after merge.

Use `Refs` for:

- partial implementation
- preparation work
- follow-up work
- promote PRs
- release PRs
- validation-only PRs
- contextual links

When multiple issues are related, explain the relationship for each issue instead of listing issue numbers only.

Recommended issue section:

```text
## Issues
- Closes #123: implements the complete task graph state machine and satisfies all acceptance criteria.
- Refs #124: documents ctxlib retrieval assumptions but does not implement runtime retrieval yet.
- Refs #125: this PR promotes previously merged dev work to main.
```

---

## 6. Rollback and migration sections

Large migrations, deployment changes, configuration changes, and data changes must also include:

```text
## Rollback
- How to revert or roll back
- Whether rollback requires configuration, data, or deployment actions
```

This repository currently does not define a database migration system. If one is introduced later, PRs involving migrations must also include:

```text
## Migration
- Which migration files were added, changed, or removed
- The latest migration number on the target branch
- Whether this PR's migration number conflicts with the target branch
- Whether it includes backfill, data repair, index creation, constraint changes, or irreversible DDL
- Whether local or CI migration validation was run
```

Migration rules, once migrations exist:

- Allocate migration numbers from the latest target branch state.
- After rebasing, if the target branch added migrations, renumber to avoid duplicates.
- New PRs must not introduce duplicate migration numbers.
- Do not casually renumber historical migrations that are already part of the baseline.
- If irreversible DDL, data deletion, large-table indexes, stricter constraints, or backfills are involved, document impact, estimated duration, and rollback limits in `Risk` and `Rollback`.
- Old green CI on GitHub does not prove a migration is currently mergeable; rerun relevant CI after rebase or renumbering.

---

## 7. Review closure

- P1/P2 findings from Codex or human review must be handled before merge, unless the owner explicitly records why the risk is accepted.
- After fixing a review comment, do not only push commits. Reply in the PR thread with what changed, then request another review by commenting:

```text
@codex review
```

- After requesting review, wait for the result if there are no other tasks. If there are other tasks, continue them and return to close out the PR review later.
- If an old review thread points to a commit fixed by a later commit, reply with the fixing commit or current implementation location, then trigger a new `@codex review`.
- Do not merge into `dev` or `main` while unresolved P1/P2 review threads remain, even if the latest review status appears clean.

---

## 8. Issue and PR status

Track issue and PR status by whether the PR is still a valid merge candidate, not only by open or closed state.

- If one PR fully satisfies an issue, use `Closes #issue`; the issue may close after merge.
- If one PR partially satisfies an issue, use `Refs #issue` and list done and remaining items.
- If multiple PRs complete one issue, each PR uses `Refs #issue`; only the final source PR that satisfies all acceptance criteria uses `Closes #issue`.
- Promote/release PRs use `Refs #issue/#source-pr`; close issues only after required validation is complete.
- Superseded PRs must be marked superseded or draft, or closed, with a PR comment such as `Superseded by #...`.
- When a PR is split, discarded, or replaced, update the related issue with the current valid PR list.

For partially completed issues, add an issue comment like:

```text
## Status
- Done: completed acceptance items
- Remaining: incomplete acceptance items
- Active PRs: current valid PRs
- Follow-ups: follow-up issues or tasks to split out
```

---

## 9. Conflict handling

Conflict types and required actions:

- Merge conflict: short-lived work branch PRs should be rebased by the author onto the latest target branch.
- Promote conflict: `dev -> main` PRs must not rebase `dev`; resolve manually, merge a source fix into `dev`, or recreate the promote PR after `dev` is corrected.
- Migration conflict: if migrations are introduced later, renumber migrations according to the latest target branch, check references and tests, and rerun migration validation.
- Review conflict: if GitHub says mergeable but P1/P2 findings or review threads are unresolved, fix them or record explicit risk acceptance, then trigger `@codex review`.
- Semantic conflict: when code has no text conflict but behavior, interface contracts, or state machines conflict, document the tradeoff in the PR and split or reorder merges if needed.
- Superseded conflict: if an open PR has been replaced by split PRs or a new approach, mark it draft or close it, and update the issue's active PR list.

Only short-lived work branches may be rebased. Permanent branches (`dev` and `main`) must not be rebased or history-rewritten.

When resolving conflicts in wiring entry files, such as service registration, route registration, provider binding, command entry points, or future agent/tool registration files, preserve both sides' registration logic. Do not keep only one side.

Promote PRs should express branch promotion only. If a promote PR includes extra hotfix commits, review it like a normal source implementation PR.

---

## 10. Sweep output

When sweeping open issues or PRs, produce output that directly tells the next action:

```text
## Sweep Result
- #<pr>: current candidate / blocked / superseded / promote / needs owner decision
- Blocking reason: merge conflict / migration conflict / review P1/P2 / missing CI / missing acceptance
- Related issues: whether Closes/Refs relationships are correct
- Next action: rebase, renumber migration, add tests, reply to review, trigger @codex review, update issue status, or close superseded PR
```

---

## 11. CI and branch protection

This repository currently appears to be early-stage and documentation-heavy. Required CI should be updated when actual app, service, package, or test commands are introduced.

CI should run on PRs to:

- `main`
- `dev`

CI should also run on pushes to:

- `main`
- `dev`

Current known checks:

```text
Branch Freshness / branch-freshness
```

Freshness CI verifies that every PR branch contains the current target branch tip in its history. For normal work branches targeting `dev`, this means the branch must be based on the latest `origin/dev`. For promote PRs targeting `main`, `dev` must contain the current `main` tip.

If freshness fails, update the short-lived branch with:

```text
git fetch origin dev
git rebase origin/dev
```

Do not rebase permanent branches (`main` or `dev`). For `dev -> main` promote freshness failures, update `dev` by merging or otherwise incorporating latest `main` through the approved release flow; never rebase `dev`.

Recommended branch protection:

Protected permanent branches:

- `main`
- `dev`

Enable protection rules:

- Require pull request before merging.
- Require status checks to pass once checks exist.
- Block direct pushes.
- Block force pushes.
- Block branch deletion.

The first version does not require merge queue, mandatory review, or required CODEOWNERS unless maintainers explicitly enable them.

---

## 12. Release and hotfix

If this repository begins publishing releases, production releases should be tag-based and tags should come from `main`.

Suggested tag format:

```text
vYYYY.MM.DD
vYYYY.MM.DD.N
```

Normal release flow:

1. Merge ready work into `dev`.
2. Validate `dev`.
3. Open a release/promote PR from `dev` to `main`.
4. Merge with merge commit.
5. Create a tag from `main` if releasing.
6. Deploy or publish from the tag if applicable.

Prefer rolling back by redeploying the previous production tag. Create a revert commit only when source code must also express the rollback state.

Temporary release branch rules:

- Use only when release freeze or long validation is needed.
- Create from `dev`.
- Accept only release blocker fixes.
- Merge to `main` when release is ready.
- Tag from `main`.
- Delete the release branch after release.

Hotfix branch format:

```text
hotfix/<scope>/<description>
```

If `main` equals the current production version, branch hotfixes from `main`. If `main` already contains unreleased changes, branch hotfixes from the latest production tag.

Normal hotfix flow:

1. Open the hotfix PR to `dev` first when urgency allows.
2. Run relevant CI and validation.
3. Open a PR from the hotfix branch or `dev` to `main`.
4. Merge into `main`.
5. Tag from `main` if releasing.
6. Forward-merge or cherry-pick back to `dev` if needed.

Emergency hotfix flow:

1. Open the hotfix PR directly to `main`.
2. Run minimum required validation.
3. Merge and tag from `main` if releasing.
4. Forward-merge or cherry-pick back to `dev`.
---

# AGENTS.md

## Engineering Standards

### 1. Scope discipline

* Keep every change small and scoped.
* Do not mix feature work, refactor, formatting, dependency changes, and test rewrites in one change unless explicitly requested.
* Do not edit unrelated files.
* Do not perform opportunistic cleanup outside the requested scope.
* Do not weaken existing behavior to make a change easier.
* If the requested change requires touching another module, explain why before doing so.

---

### 2. Package boundaries

* Use package boundaries as hard engineering boundaries.
* Do not import another package's private or internal implementation.
* Do not create reverse dependencies to make code compile.
* Do not introduce cyclic dependencies.
* Prefer public interfaces over direct implementation coupling.
* If a public interface is insufficient, propose an interface change instead of bypassing the boundary.

---

### 3. Dependency direction

* Higher-level orchestration may depend on lower-level packages.
* Lower-level domain packages must not depend on concrete adapters, UI, CLI entrypoints, or bootstrap code.
* Adapter packages should depend on stable interfaces and platform utilities, not on orchestration internals.
* Bootstrap/wiring code is the only place that should assemble concrete implementations.
* Avoid hidden dependency paths through shared utility packages.

---

### 4. Public API design

* Keep public APIs small, explicit, and stable.
* Export only what is required by other packages.
* Prefer narrow interfaces over large all-purpose interfaces.
* Define interfaces near the consumer when practical.
* Do not expose provider-specific, storage-specific, or transport-specific details through generic APIs.
* Public types and functions should have clear names and comments.
* Avoid vague names such as `Manager`, `Processor`, `Helper`, `Util`, or `Common` unless the ownership is obvious.

---

### 5. Error handling

* Return explicit errors.
* Do not ignore errors.
* Do not hide important error context.
* Do not panic for normal control flow.
* Wrap errors with useful context when crossing package boundaries.
* Prefer typed or sentinel errors when callers need to make decisions.
* Tests should cover important error paths, not only success paths.

---

### 6. Context and cancellation

* Long-running, blocking, IO, process, network, and storage operations should accept `context.Context`.
* Respect cancellation and deadlines.
* Do not create background goroutines without a clear shutdown path.
* Do not leak processes, file handles, timers, channels, or goroutines.
* Use `defer` for cleanup when appropriate.

---

### 7. Concurrency

* Make shared mutable state explicit.
* Protect shared state with the appropriate synchronization primitive.
* Avoid global mutable state.
* Avoid data races.
* Prefer simple ownership and message passing over clever shared-state designs.
* Run race tests for code touching scheduling, processes, storage, event streams, or shared state.

---

### 8. Testing

* Add or update tests for changed behavior.
* Do not delete or weaken tests to make a change pass.
* Prefer table-driven tests for state transitions, parsers, policies, and validation logic.
* Use contract tests for interface implementations.
* Use integration tests only where package-level tests cannot prove the behavior.
* Keep tests deterministic.
* Avoid relying on wall-clock timing unless the behavior is explicitly time-based.
* Mock external processes, filesystem, and network boundaries when possible.

---

### 9. Go toolchain checks

Before considering a change complete, run or preserve compatibility with:

```text
gofmt
go vet ./...
go test ./...
go test -race ./...
go mod tidy
architecture dependency check
```

Rules:

* Code must be gofmt-formatted.
* Code must compile without unused imports or unused variables.
* `go vet` findings should be fixed, not suppressed.
* `go mod tidy` should not introduce unrelated dependency churn.
* Dependency boundary checks must remain clean.

---

### 10. Dependency management

* Do not add third-party dependencies without clear justification.
* Prefer the standard library when it is sufficient.
* Keep dependencies narrow and replaceable.
* Do not add a dependency for a small helper function.
* Do not update unrelated dependencies.
* Any change to `go.mod` or `go.sum` must be intentional.

---

### 11. File organization

Preferred package layout:

```text
types.go        shared domain types
ports.go        interfaces / boundaries
service.go      main package behavior
errors.go       package errors
*_test.go       tests
internal/       private implementation
```

Rules:

* Organize files by ownership and responsibility, not by arbitrary technical categories.
* Keep generated files clearly marked.
* Do not place business logic in `cmd/`.
* Do not place unrelated shared logic in catch-all packages.
* Avoid large files that mix unrelated responsibilities.

---

### 12. Naming

* Names should describe ownership and intent.
* Prefer concrete domain names over generic technical names.
* Avoid vague packages such as `common`, `utils`, `helpers`, `misc`, `core`, or `engine`.
* Avoid vague types such as `Data`, `Info`, `Payload`, `Manager`, or `Processor` when a more specific name exists.
* Keep acronyms consistent.
* Avoid abbreviations unless they are standard in the codebase.

---

### 13. Logging and observability

* Log important boundaries, failures, and externally visible state changes.
* Do not log secrets, credentials, tokens, private keys, or sensitive user data.
* Keep logs structured where possible.
* Do not use logs as a substitute for returned errors.
* Do not leave temporary debug prints in committed code.

---

### 14. Configuration

* Keep configuration explicit.
* Do not hide behavior behind magic defaults.
* Defaults should be safe and conservative.
* Validate configuration at startup or construction boundaries.
* Do not read environment variables deep inside domain packages.
* Pass configuration through constructors or explicit dependency wiring.

---

### 15. Security and safety

* Treat shell execution, filesystem writes, network access, and credential access as high-risk boundaries.
* Validate paths before using them.
* Avoid path traversal vulnerabilities.
* Do not print or persist secrets.
* Prefer allowlists over denylists for dangerous capabilities.
* Fail closed when permissions, capabilities, or configuration are ambiguous.

---

### 16. Generated code

* Generated code must be reproducible.
* Commit generator inputs alongside generated outputs.
* Mark generated files clearly.
* Do not manually edit generated files unless the generator workflow explicitly allows it.
* Keep generation commands discoverable through `go generate`, Makefile targets, or scripts.

---

### 17. Review discipline

* A change should be easy to review.
* Summaries should state what changed, why, and how it was verified.
* Call out risky areas explicitly.
* Call out behavior changes explicitly.
* Call out dependency changes explicitly.
* Call out skipped checks explicitly.

---

### 18. Completion criteria

A change is not complete until:

* The requested behavior is implemented.
* Relevant tests are added or updated.
* Existing tests still pass.
* Formatting is clean.
* Dependency changes are intentional.
* Architecture boundaries are not violated.
* No temporary debug code remains.
* The final summary includes verification performed.

---

# Code Aesthetic Standards and Architecture Aesthetic Standards

## 1. Code Aesthetic Standards

Good code is not "clever". Good code is clear, stable, modifiable, and verifiable.

### 1. Clear intent

Code should make it obvious what problem it solves.

Good code:

* Names directly express domain meaning.
* Functions have one responsibility.
* The call chain reads like a business process.
* A function can be understood without reading the entire file.
* Important decisions are explicit and not hidden behind tricks.

Bad smells:

* Names are too generic: `handle`, `process`, `manager`, `data`, `info`.
* A function does too many things.
* Understanding what code really does requires frequent jumping across files.
* Cleverness is used instead of expression.

Aesthetic judgment:

```text
If a function cannot be described in one sentence, it is probably too large.
```

---

### 2. Clear boundaries

Every piece of code should know where it belongs and what it must not own.

Good code:

* A module only operates on data it owns.
* Cross-module calls go through public interfaces.
* It does not steal another module's internal implementation.
* It does not bypass boundaries for convenience.
* If an interface is insufficient, it proposes an interface change instead of forcing changes into another module.

Bad smells:

* `internal` packages are imported everywhere.
* One module directly mutates another module's state.
* A giant `utils` package is created to reuse a small piece of logic.
* A lower-level module depends back on a higher-level module.

Aesthetic judgment:

```text
Crossing code boundaries is usually not a small issue; it is the beginning of architectural decay.
```

---

### 3. Types and data structures express real semantics

Data structures should express real system concepts, not temporary bags of fields.

Good code:

* Type names correspond to domain objects.
* State enums are finite and explicit.
* Input and output structures are clear.
* Different meanings are not mixed into the same field.
* Critical state is not passed as bare strings.

Bad smells:

* `map[string]any` is passed everywhere.
* Everything is called `Payload`.
* State strings are scattered across the codebase.
* Too many booleans combine into unclear semantics.
* One struct is simultaneously a database model, API response, and internal state.

Aesthetic judgment:

```text
Types should reduce explanation cost, not push explanation onto comments and callers.
```

---

### 4. Direct control flow

Good code keeps the main path clear and error paths explicit.

Good code:

* Errors and boundary cases are handled first.
* The main flow stays flat.
* Nesting is limited.
* Hidden side effects are minimized.
* State changes have explicit entry points.

Bad smells:

* Three or four levels of nesting.
* Shared state is mutated throughout one function.
* Early returns and side effects are mixed unpredictably.
* Error paths swallow errors.
* Execution continues after failure.

Aesthetic judgment:

```text
The main flow should look like a straight line; error paths should look like clear exits.
```

---

### 5. Informative error handling

Errors do not exist merely to stop a program; they exist to tell people what broke.

Good code:

* Errors include context.
* Errors are not swallowed.
* Not every error is wrapped into the same vague error.
* Callers can make decisions based on errors.
* Tests cover critical error paths.

Bad smells:

* `return err` without context.
* `panic` is used for normal failures.
* Errors are ignored.
* An error is logged, but the return value says success.
* Error messages say only "failed" without saying what failed.

Aesthetic judgment:

```text
A good error message should save the next person ten minutes.
```

---

### 6. Tests prove behavior, not implementation details

Tests should protect system behavior, not bind the code to a temporary implementation.

Good code:

* Tests focus on inputs, outputs, state transitions, and error paths.
* Critical interfaces have contract tests.
* State machines, parsers, and policies use table-driven tests.
* Tests are stable and repeatable.
* Test failures point to behavior problems.

Bad smells:

* Only the happy path is tested.
* Internal functions are heavily mocked.
* Tests depend on execution order or sleeps.
* Behavior is weakened to make tests pass.
* Test names do not describe scenarios.

Aesthetic judgment:

```text
Tests should make refactoring safer, not harder.
```

---

### 7. Dependency restraint

Every dependency adds long-term maintenance cost.

Good code:

* The standard library is preferred when sufficient.
* The benefit is explicit before a dependency is introduced.
* Dependencies are concentrated at boundaries.
* Core logic does not depend on concrete external services.
* Dependencies are replaceable.

Bad smells:

* A large library is added for a small helper function.
* Core domain code depends on an SDK.
* Unrelated dependencies are updated.
* Code implicitly depends on environment variables, global config, or external state.
* Dependencies make tests harder.

Aesthetic judgment:

```text
A dependency should buy capability, not a tiny bit of convenience.
```

---

### 8. Code should be deletable

Good code is not better because there is more of it; good code has no extra weight.

Good code:

* There is no dead code.
* There are no unused abstractions.
* There is no large framework built for "maybe later".
* Removing a feature has a controlled impact radius.
* Old paths are removed after new paths replace them.

Bad smells:

* Interfaces exist because they "might be useful later".
* Empty implementations, fake implementations, or half-finished code remain.
* Multiple equivalent paths coexist.
* Comments say TODO, but the code looks complete.
* Compatibility layers have no exit plan.

Aesthetic judgment:

```text
Truly advanced code often exists because half of it was not written.
```

---

## 2. Architecture Aesthetic Standards

Good architecture is not about complex diagrams. Good architecture has stable responsibility, one-way dependencies, and localized change.

### 1. Architecture should center on invariants, not feature lists

Features change; invariants are relatively stable.

Good architecture:

* It first identifies which rules must not be broken.
* Core modules are designed around invariants.
* External changes are absorbed through adapters.
* New features do not require frequent changes to the core state machine.
* Critical state changes have a single entry point.

Bad smells:

* Every new requirement adds another `if`.
* Core rules are scattered across layers.
* Every module can mutate critical state.
* There is no clear source of truth.
* More features make boundaries more ambiguous.

Aesthetic judgment:

```text
Architectural beauty comes from stable invariants, not a pretty directory tree.
```

---

### 2. Dependencies should be one-way

The most important architectural quality is direction.

Good architecture:

* Higher layers orchestrate; lower layers execute.
* The core does not depend on external implementations.
* Adapters depend on interfaces and are not directly depended on by the core.
* Bootstrap code assembles concrete implementations.
* There are no cyclic dependencies.

Bad smells:

* The domain depends on CLI, database, or SDK code.
* An adapter calls back into the scheduler.
* UI directly mutates core internal state.
* Reverse dependencies are created to share code.
* Two modules know too much about each other.

Aesthetic judgment:

```text
If dependency direction is wrong, locally clean code cannot save the system.
```

---

### 3. Small core, thick boundaries

The core should express stable rules; the messy external world belongs at the boundaries.

Good architecture:

* The core model is small and stable.
* Adapters do the dirty work: protocols, commands, parsing, permissions, and external formats.
* The core only sees unified events, unified results, and unified interfaces.
* External implementation changes do not pollute the core.
* Provider-specific details do not float upward.

Bad smells:

* External tool parameters appear in the core.
* Every new provider requires changing the core.
* Business state depends on an SDK response shape.
* Adapters are thin shells while messy logic leaks upward.
* The system forcibly erases all capability differences for the sake of uniformity.

Aesthetic judgment:

```text
The core should be clean. Boundaries may be dirty, but the dirt must be contained.
```

---

### 4. Modules have owners

Every state, rule, and piece of data should have one owner.

Good architecture:

* It is clear who can write each kind of state.
* Boundaries define who can read, who can mutate, and who can derive.
* Cross-module interaction happens through requests or events.
* No two modules own the same fact.
* Projections are clearly separated from sources of truth.

Bad smells:

* Multiple modules can mutate the same state.
* There is no single source of truth.
* Caches, projections, and real data are mixed together.
* State updates depend on an assumed order.
* When bugs happen, nobody knows which module should change.

Aesthetic judgment:

```text
One fact can have only one owner; everyone else can only reference or derive it.
```

---

### 5. Interfaces express semantics, not implementation

Interfaces should describe the capability needed, not how the lower layer implements it.

Good architecture:

* Interface names come from domain language.
* Parameters express intent, not external command flags.
* Return values express results the system can consume.
* Implementation details stay inside adapters.
* Capabilities are declared through explicit structures.

Bad smells:

* Upper-level interfaces expose CLI flags.
* Domain types contain database fields.
* Transport schemas are used directly as internal models.
* One interface keeps growing to fit many implementations.
* Callers must know implementation details to use an interface correctly.

Aesthetic judgment:

```text
A good interface lets callers know less; a bad interface forces callers to know everything.
```

---

### 6. State machines are explicit

Any system with a workflow should explicitly express states and transitions.

Good architecture:

* The state set is finite.
* Legal transitions are explicit.
* Illegal transitions fail.
* Blocked, failed, and completed are not collapsed into the same state.
* State changes have events and evidence.

Bad smells:

* State is assembled from several booleans.
* `status = "running"` hides many sub-meanings.
* Any module can mutate state.
* Failure and waiting are not distinguished.
* State changes are not recorded.

Aesthetic judgment:

```text
A state machine is not an implementation detail; it is a system contract.
```

---

### 7. Concurrency relies on isolation, not hope

Concurrent architecture should first prevent shared-state chaos.

Good architecture:

* Concurrent tasks have isolated execution spaces.
* Shared state has clear synchronization.
* Conflict detection is based on real observation.
* Completed results are preferred; unfinished work adapts.
* Concurrency changes throughput, not semantics.

Bad smells:

* Multiple workers directly mutate the same state.
* Conflicts are tracked by human memory.
* There is no observed write set.
* Higher concurrency makes results less predictable.
* After failure, nobody knows which worker did what.

Aesthetic judgment:

```text
Good concurrent architecture makes concurrency faster; bad concurrent architecture makes concurrency random.
```

---

### 8. Traceability

The system should be able to look back at what it did, why it did it, and what evidence it used.

Good architecture:

* Critical actions have events.
* Large objects have artifacts.
* Decisions have evidence.
* Results can be linked to inputs, execution, verification, and merge.
* Projections can be rebuilt from sources of truth.

Bad smells:

* There is only final state, not process.
* Failure reasons exist only in log fragments.
* Human decisions are not recorded.
* Large outputs are stored directly in state tables.
* A result cannot be explained after the fact.

Aesthetic judgment:

```text
Automation that cannot be traced cannot be governed.
```

---

### 9. Extension points are few and precise

Architecture should allow change, but it should not put plugin points everywhere.

Good architecture:

* Extension points correspond to real axes of change.
* Each extension point has a stable contract.
* The default implementation is simple.
* New implementations can be verified with the same tests.
* Extensions do not require changes to the core model.

Bad smells:

* A dozen interfaces are reserved for needs that have not happened.
* Everything is configurable.
* Plugins can bypass core rules.
* The abstraction is more complex than the implementation.
* A new extension requires understanding the entire system.

Aesthetic judgment:

```text
A good extension point is like a door; a bad extension point is like holes all over the wall.
```

---

### 10. MVP loop first

Architecture should not be built all at once. First, complete the smallest credible end-to-end loop.

Good architecture:

* There is an end-to-end loop first.
* Every module has a minimal usable implementation.
* Advanced capabilities attach to clear extension seams.
* Complex systems are not used to hide the absence of a working core loop.
* Every step can be verified.

Bad smells:

* The full plugin system is built first.
* Complex UI is built first.
* Advanced memory, ranking, or marketplace systems are built first.
* Optimization starts before the core state flow works.
* TODOs and fake implementations are everywhere.

Aesthetic judgment:

```text
Architecture's first virtue is closing the loop; its second virtue is extensibility.
```

---

## 3. Quick Judgment Checklist

### Good code usually looks like this

```text
Accurate names
Short functions
Clear boundaries
Explicit errors
Trustworthy tests
Restrained dependencies
No hidden side effects
No overly clever tricks
```

### Bad code usually looks like this

```text
Managers / helpers / utils everywhere
Private imports across modules
Every change affects everything
State strings scattered everywhere
Errors swallowed
Only happy paths tested
Dependencies keep increasing
Behavior can only be guessed by reading the whole project
```

### Good architecture usually looks like this

```text
Small core
Hard boundaries
One-way dependencies
Single source of truth
Explicit state machines
Few and precise extension points
Critical behavior is traceable
Concurrency is isolated
Verification has a gate
```

### Bad architecture usually looks like this

```text
Many directories but unclear responsibilities
Modules know too much about each other
Core depends on external tools
Everyone can mutate critical state
No single source of truth
State and events are confused
Many abstractions but a weak loop
New features require changes everywhere
```

---

## 4. Final Aesthetic Principles

```text
Code aesthetics: make local behavior understandable at a glance.

Architecture aesthetics: prevent system changes from spreading.

Engineering aesthetics: expose errors early, make boundaries enforce themselves, and make verification the default path.
```

---

## 5. Ponytail-Derived Minimalism Rules

These rules are adapted from Dietrich Gebert's Ponytail project and apply as an additional anti-overengineering layer. They are meant to make work smaller, not sloppier: understand the real flow first, then choose the smallest correct change.

### 1. Climb the minimal-change ladder before writing code

Before adding code, check the rungs in order and stop at the first one that solves the problem:

1. Does this need to exist, or is it speculative?
2. Does this codebase already have a helper, type, utility, or pattern for it?
3. Does the standard library already solve it?
4. Does the platform, framework, database, shell, browser, or OS already provide it?
5. Does an already-installed dependency cover it well enough?
6. Can the whole change be a direct one-liner or tiny local edit?
7. Only then write the minimum new code that works.

### 2. Reuse, delete, and shrink before adding

* Prefer deleting unused paths over adding new layers.
* Prefer existing code paths over duplicated logic.
* Prefer boring direct code over clever indirection.
* Prefer fewer files, fewer moving parts, and the shortest reviewable diff that is still in the correct location.
* Do not add scaffolding, factories, plugin systems, registries, or configuration for hypothetical future needs.

### 3. Fix root causes, not named symptoms

A bug report usually names the visible failure, not the owner of the defect. Before changing a function or shared path, inspect its callers and siblings. A single correct fix in the shared owner is usually smaller and safer than local guards scattered across call sites.

### 4. Delay abstraction until evidence exists

* Do not introduce an interface with one implementation.
* Do not create generic managers, processors, helpers, or factories for one concrete use case.
* Do not make a value configurable unless someone actually configures it.
* Let real second use cases, contract tests, or package boundaries justify abstraction.

### 5. Prefer native and standard capabilities

Reach for built-in language, platform, framework, database, and shell features before custom code or new dependencies. A dependency must buy meaningful capability, safety, interoperability, or maintenance leverage; it should not be added for a small helper or a thin wrapper around native behavior.

### 6. Keep validation, security, and correctness non-negotiable

Minimalism does not justify weak validation, swallowed errors, data-loss risk, security shortcuts, accessibility regressions, or unverified hardware/platform assumptions. When two simple approaches are available, choose the one that handles edge cases correctly.

### 7. Leave one small runnable check for non-trivial logic

For meaningful branches, loops, parsers, policies, money/security paths, or bug fixes, leave the smallest useful check that would fail if the behavior regresses. Prefer a focused unit test, contract test, smoke test, or assert-based self-check over a large testing framework addition.

### 8. Mark intentional shortcuts explicitly

If a simplification has a known ceiling, mark it with a `ponytail:` comment that states both the ceiling and the upgrade path. Examples of ceilings include a global lock, an O(n^2) scan, a naive heuristic, or a deliberately narrow adapter.

### 9. Report skipped complexity briefly

When delivering a minimal solution, state what was intentionally skipped and when it should be added. Keep the explanation short unless the user requested a deeper design note or walkthrough.

