# Working rules

## Subagents

- Always use subagents whenever possible. Delegate clearly bounded work and run
  independent work in parallel when it does not conflict.
- Keep responsibility for integration and verification with the coordinating
  agent; delegation does not replace checking the results.

## Incremental implementation

- Try to break each implementation slice into logical, incremental,
  self-contained subslices. Each subslice should have a clear outcome and
  its own validation.
- Complete and validate one subslice before moving to the next. Each subslice
  must be an independent commit; do not bundle multiple subslices into one commit.

## Independent review

- At the end of every subslice, run an independent review subagent over the
  codebase, including the changes and their surrounding design and integration.
  Ask it to identify problems, mistakes, regressions, and design issues.
- Resolve all review feedback before committing the completed subslice and
  moving on to the next subslice or slice. Re-run affected checks after fixes.

## Implementation log

- Create and maintain a running implementation log in the repository so the
  user can see which slices and subslices are completed, in progress, and
  coming up next.
- Update it as work progresses and sequencing changes. Keep it concise and
  human-readable; mark work complete only after implementation, validation,
  and independent review are finished.

## Decision log

- While implementing each subslice, maintain a running decision log recording
  the decisions made and why. Keep it concise and human-readable;
  avoid transcripts, routine command logs, and repeated plan text.

## Simple, idiomatic Go

- Prefer the standard library. Add dependencies, interfaces, wrappers, and
  configuration only when they solve a concrete need, not hypothetical future
  requirements.
- Prefer many small, focused Go files over a few large files. Organize files
  by coherent responsibility, keeping closely related code together.

## Executable verification

- Format Go changes with `gofmt`. Before completing an implementation subslice,
  run `go test ./...`, `go test -race ./...`, and `go vet ./...`.
- Add regression tests for bugs. Report blocked or failing checks honestly.

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
  See [the implementation plan](docs/plans/https-registry-cache.md) for detailed
  protocol rules.

## Bounded resources and safe failure

- Stream layers rather than buffering them in memory. Give goroutines,
  connections, response bodies, and temporary files clear ownership and
  cleanup paths.
- Test cancellation, interrupted downloads, disk exhaustion, and concurrency.
  Never publish incomplete cache entries.

## Documentation alignment

- Update the plan, configuration examples, and operating instructions when
  behavior changes.
- Ask before materially changing agreed scope or security guarantees; do not
  quietly substitute a different design.

## Commit messages

- Do not add Amp branding or attribution to commit subjects, bodies, or trailers.
