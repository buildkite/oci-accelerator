# Working rules

## Subagents

- Always use subagents whenever possible. Delegate clearly bounded work and run
  independent work in parallel when it does not conflict.
- Only the coordinating agent delegates unless nested delegation is explicitly
  requested. Workers and reviewers must not recursively spawn more subagents.
- Keep responsibility for integration and verification with the coordinating
  agent. Delegation does not replace checking the results.

## Incremental implementation

- Try to break each implementation slice into logical, incremental,
  self-contained subslices. Each subslice should have a clear outcome and
  its own validation.
- Complete and validate one subslice before moving to the next. Each subslice
  must be an independent commit. Do not bundle multiple subslices into one commit.

## Independent review

- At the end of every implementation subslice, use two fresh, read-only review
  subagents that did not implement it. Both must review the same source, tests,
  and code-diff snapshot. This two-review gate does not apply to routine editorial
  changes outside implementation subslices.
- Give the first reviewer repository and task context. Ask it to identify
  problems, mistakes, regressions, and design issues across the changes and
  their surrounding code, and assess the implementation against the agreed scope.
- Focus the second reviewer only on code quality, logic, bugs, and design.
  Provide a filtered workspace or input bundle containing source, tests,
  fixtures, build/module files, and code diffs, but no documentation or Git
  history. Do not supply normal repository context that automatically exposes
  `AGENTS.md`, plans, logs, design explanations, or the first reviewer's findings.
  Instruct it not to read those materials or infer requirements that the code
  does not establish. If the tooling cannot provide this separation, disclose
  the limitation rather than claiming a blind review.
- The code-only review complements, rather than replaces, the first review's
  assessment of intended behavior and requirements.
- Resolve feedback from both reviewers before committing the completed subslice
  or moving on: fix actionable findings and briefly explain rejected or
  inapplicable findings. Do not silently discard feedback.
- Re-run affected checks after fixes. Material behavior or design changes made
  during review must receive follow-up independent review under the same context
  rules before committing the completed subslice or moving on.

## Simple, idiomatic Go

- Prefer the standard library. Add dependencies, interfaces, wrappers, and
  configuration only when they solve a concrete need, not hypothetical future
  requirements.
- Prefer many small, focused Go files over a few large files. Organize files
  by coherent responsibility, keeping closely related code together.
- Name test files `<owner>[_<topic>]_test.go` after the source file or
  responsibility they verify, not the fixture or entry point they call. Keep
  shared fixtures in `test_helpers_test.go`, and keep helpers used by a single
  suite in that suite's file.
- Order declarations within a file in newspaper style: the most important,
  highest-level declarations first, with supporting detail beneath. Types and
  entry points lead, callers precede callees, and private helpers sink below
  the code that uses them.

## Executable verification

- Format Go changes with `gofmt`. Before completing an implementation subslice,
  run `go test ./...`, `go test -race ./...`, and `go vet ./...`.
- Add regression tests for bugs. Report blocked or failing checks honestly.
- If a required check fails or cannot run, report the blocker to the user
  and keep the subslice incomplete. Do not advance past it
  until the blocker is resolved or the user explicitly grants an exception.

## Behavioral tests

- Use deterministic local registry, authentication, and CDN fixtures.
- Assert returned bytes, authorization outcomes, and upstream request counts,
  not merely log messages. Cache tests must distinguish proxy hits from Docker's
  own local cache.

## Security before cache hits

- Preserve per-request upstream GET authorization and fail closed.
- Never disable upstream TLS verification, log credentials, or forward
  credentials across origins.
- Changes to these security boundaries require explicit design approval.
  See [how it works](docs/how-it-works.md), the
  [operating guide](docs/operations.md), and source comments for detailed
  protocol rules.

## Bounded resources and safe failure

- Stream layers rather than buffering them in memory. Give goroutines,
  connections, response bodies, and temporary files clear ownership and
  cleanup paths.
- Test cancellation, interrupted downloads, disk exhaustion, and concurrency.
  Never publish incomplete cache entries.

## Documentation alignment

- Keep documentation about the current code, not its delivery history. Update
  architecture, design rationale, configuration examples, and operating instructions when
  behavior changes.
- Keep reproducible verification commands and current limitations. Do not maintain
  implementation/decision logs, completed-slice narratives, or superseded alternatives.
- Ask before materially changing agreed scope or security guarantees. Do not
  quietly substitute a different design.

## Writing style

- Do not use semicolons in documentation, code comments, user-facing
  messages, or other prose. Split the sentence, or join the clauses with a
  conjunction or colon. Semicolons required by code syntax are fine.

## Commit messages

- Push each commit to `origin` immediately after creating it. Do not accumulate
  completed commits only in the local checkout.
- Give every commit a readable summary of the changes it implements in the
  commit body, and summarize those changes when reporting the commit to the user.
- Do not add Amp branding or attribution to commit subjects, bodies, or trailers.
