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

## Decision log

- While implementing each subslice, maintain a running decision log recording
  the decisions made and why. Keep it concise and human-readable;
  avoid transcripts, routine command logs, and repeated plan text.

## Commit messages

- Do not add Amp branding or attribution to commit subjects, bodies, or trailers.
