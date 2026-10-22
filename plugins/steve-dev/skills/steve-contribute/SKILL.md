---
name: steve-contribute
description: Make a change to the Steve repository (gopact-ai/steve) and open a pull request that passes its gates.
---

## Layout

- `cmd/steve`, `cmd/steve-node`: the two binaries. `cmd/steve/main.go` is a
  composition root; `internal/architecture` limits its size and imports.
- `internal/...`: Go packages. `web/console`: the React/Vite console, built into
  `internal/readmodel/web/dist` and embedded in the binary.
- `providers/`: event providers that run as separate processes. `plugins/`:
  first-party capability packages. `docs/`: user and operator documentation.

## Workflow

1. Start from an up-to-date `master` on a new branch. One pull request is one
   coherent change; put follow-ups in separate pull requests.
2. While editing, run `gofmt` and compile. Before opening the pull request run
   the gates for what changed:
   - Go: `go vet ./...` and `go test -race` for the touched packages; `make test`
     for cross-cutting changes.
   - Console: `npm --prefix web/console ci`, `npm --prefix web/console run build`,
     then the matching `test:*` scripts in `web/console/package.json`. Commit the
     rebuilt `internal/readmodel/web/dist`; CI fails if it differs from a clean
     `npm ci && npm run build`.
   - Delegation, landing, timeouts, snapshots or node channels: also
     `make e2e-fleet` and the live fleet gate in `CONTRIBUTING.md`, with the full
     output in the pull request.
3. New tests assert a requirement or reproduce a concrete failure. Do not add
   tests that only mirror forwarding code.
4. Pull request description: summary, what was tested with the commands run,
   and anything not verified. Public text (README, docs, PR titles) describes
   behavior, not internal plans or review notes.
5. Steve uses merge commits. Merging requires the owner's approval.

## Before changing an area

Read `CONTRIBUTING.md`, `docs/operations.md` and `web/console/AGENTS.md` for
the parts you touch. Address review comments with a code change or a short
factual reply, then resolve the thread.
