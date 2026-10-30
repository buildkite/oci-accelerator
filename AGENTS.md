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
- A small, coherent change may be a single subslice. Split larger changes where
  each part can be understood and validated independently.
- Complete and validate one subslice before moving to the next. Each subslice
  must be an independent commit. Do not bundle multiple subslices into one commit.

## Independent review

- At the end of every implementation subslice, use two fresh, read-only review
  subagents that did not implement it. Both must review the same production-code
  snapshot. This two-review gate does not apply to routine editorial
  changes outside implementation subslices.
- Give the first reviewer repository and task context, tests, and code diffs.
  Ask it to identify
  problems, mistakes, regressions, and design issues across the changes and
  their surrounding code, and assess the implementation against the agreed scope.
- Focus the second reviewer only on code quality, logic, bugs, and design.
  Provide a filtered workspace or input bundle containing only production source
  files under review and relevant surrounding production code. Exclude tests,
  fixtures, build/module files, code diffs, documentation, Git history, commit
  messages, plans, logs, design explanations, and the first reviewer's findings.
  Supply only minimal review instructions, without task rationale, intended
  behavior, or other explanatory prose. Do not supply normal repository context
  that automatically exposes `AGENTS.md` or any excluded materials.
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
- Follow-up reviews may use the original reviewers. Both must review the same
  updated production-code snapshot, with their original context boundaries
  preserved.

## Simple, idiomatic Go

- Prefer the standard library. Add dependencies, interfaces, wrappers, and
  configuration only when they solve a concrete need, not hypothetical future
  requirements.
- Prefer many small, focused Go files over a few large files. Organize files
  by coherent responsibility, keeping closely related code together.
- Use one blank line between logical code blocks when the purpose changes.
  Keep closely related statements together, including an operation and its
  immediate error check, resource acquisition and its cleanup `defer`, and a
  lock with its protected updates and unlock. Keep comments attached to the
  code they describe. Do not separate every statement or brace-delimited block.
- After `gofmt`, explicitly review every added or edited Go function for these
  logical-block separators before considering the change ready. `gofmt` and
  `git diff --check` do not enforce this rule. Include this spacing check in
  review requests, and inspect the final code rather than only the diff hunks.
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
- For every implementation subslice, review the affected tests for redundancy,
  necessity, and clarity. Remove tests that add no distinct regression protection.
  Refactor tests where doing so reduces duplication or unnecessary machinery
  without weakening assertions or obscuring intent. Remove fixtures and helpers
  that are no longer needed. Preserve coverage of distinct behavior and failure
  modes.

## Security before cache hits

- Preserve per-request upstream GET authorization and fail closed.
- Never disable upstream TLS verification, log credentials, or forward
  credentials across origins.
- Changes to these security boundaries require explicit design approval.
  See [how it works](docs/how-it-works.md), the
  [deployment guide](docs/deployment.md), and source comments for detailed
  protocol rules.

## Bounded resources and safe failure

- Stream layers rather than buffering them in memory. Give goroutines,
  connections, response bodies, and temporary files clear ownership and
  cleanup paths.
- Test cancellation, interrupted downloads, disk exhaustion, and concurrency.
  Never publish incomplete cache entries.

## Documentation alignment

- Keep documentation about the current code, not its delivery history. Update
  architecture, design rationale, configuration examples, and deployment instructions when
  behavior changes.
- Keep reproducible verification commands and current limitations. Do not maintain
  implementation/decision logs, completed-slice narratives, or superseded alternatives.
- Ask before materially changing agreed scope or security guarantees. Do not
  quietly substitute a different design.

## Writing style

- Do not use semicolons in documentation, code comments, user-facing
  messages, or other prose. Split the sentence, or join the clauses with a
  conjunction or colon. Semicolons required by code syntax are fine.
- In user-facing documentation, address the reader as "you". Refer to the
  person deploying the proxy as "the user" in code comments, not "the
  operator". Prefer "use" or "deploy" over "operate".
- Use American spelling and plain words. Prefer "who created it" over
  "provenance" and "isn't" over "is not" in user-facing prose.

## Documentation structure

- Keep each fact in one place and link to it elsewhere. `docs/deployment.md`
  covers setup, settings, operations, and troubleshooting.
  `docs/how-it-works.md` explains mechanics and security reasoning.
  `docs/peer-protocol.md` holds peer wire formats and protocol rules.
  `docs/settings.md` lists every environment variable, and `docs/metrics.md`
  lists every metric.
- Before deleting a duplicate, check whether it holds details the other copy
  lacks. Move those details to the canonical place instead of dropping them.
- Keep internal implementation detail, such as buffer handling or logical
  clocks, out of user documentation unless it changes what the reader sees or
  must do. State the operational effect instead.
- Order the deployment guide by the reader's path: security model, getting
  started, running and maintaining the proxy, then reference material,
  troubleshooting, and limitations. Order settings tables like `.env.example`.
- When moving, renaming, or removing a heading, update every incoming link in
  `docs/` and `README.md`, and check that all relative links and anchors
  resolve.

## Documentation formatting

- Wrap prose and list items at 80 columns. Code, tables, and long URLs are
  exempt.
- Keep paragraphs short. Split a paragraph that covers more than one idea or
  runs past about six lines. Prefer subheadings with short paragraphs over
  long bold-led bullets.
- Merge bullets that make the same point, and give each bullet one idea. Lead
  with the instruction or outcome, then the detail.
- Use compact table syntax (`| --- |`) without column padding. Use real names
  in tables, such as log fields, metric names, and environment variables.
  Drop a column whose value is the same in every row, and state it once in
  prose.
- Make code blocks copy-pasteable. Give each platform its own block rather than
  one block with platform variables. Separate logical steps inside a block with
  blank lines, and keep a comment attached to the command it describes.
- Never place a line of `=` or `-` directly under prose, because Markdown turns
  the preceding line into a heading.

## Documentation accuracy

- Verify every behavior claim against the code before writing or rewording it,
  including limits, defaults, field names, metric names, and error behavior.
  Test commands and examples when you can.
- Do not overstate guarantees, especially security ones. Scope a claim to the
  component it applies to, such as startup rather than every command.
- Mark third-party behavior you can't verify, such as Docker's, as tentative
  or link to its documentation.

## Commit messages

- When pushing is authorized, push each completed commit to `origin` immediately
  after creating it. Otherwise, keep it local and report that pushing awaits
  approval.
- Give every commit a readable summary of the changes it implements in the
  commit body, and summarize those changes when reporting the commit to the user.
- Do not add Amp branding or attribution to commit subjects, bodies, or trailers.
