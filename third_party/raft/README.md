# Local hashicorp/raft

This directory is `github.com/hashicorp/raft` v1.7.3 with the local changes
listed below. The root `go.mod` replaces the module with this directory:

```
replace github.com/hashicorp/raft v1.7.3 => ./third_party/raft
```

## Changes from v1.7.3

1. Snapshot-boundary AppendEntries validation (`raft.go`, `appendEntries`).
   When `PrevLogEntry` equals the index of the last snapshot, the follower
   takes the predecessor term from that snapshot instead of reading a log
   entry that compaction has removed. A leader that retransmits a batch
   after losing the response is then accepted instead of rejected.
   Guarded by `TestRaft_AppendEntriesSnapshotBoundaryLostACK` and
   `TestRaft_AppendEntriesSnapshotBoundaryValidation`
   (`snapshot_boundary_test.go`, added locally).

2. Commitment keeps a promoted server's match index (`commitment.go`).
   `appendConfigurationEntry` dispatches a configuration entry, which wakes
   replication, before it calls `commitment.setConfiguration`. When the
   entry promotes a nonvoter or staging server, that server can
   acknowledge the entry before it votes. v1.7.3 dropped the
   acknowledgement and started the new voter at zero, so a configuration
   whose quorum needs the promoted server did not commit until another
   entry was replicated to it. `commitment` now records the match index of
   every server in the configuration and computes the commit index from
   voters only; a demoted voter's match is kept but no longer counts.
   Guarded by `TestCommitment_promotedServerKeepsEarlierMatch` and
   `TestCommitment_demotedVoterKeepsMatch` (`commitment_test.go`); the
   other `TestCommitment_` tests are from v1.7.3.

3. Size-scaled AppendEntries deadlines (`net_transport.go`,
   `AppendEntries` and `netPipeline`). An AppendEntries request, and a
   pipelined one's write and response, get the fixed timeout plus one
   timeout for every `TimeoutScale` bytes of entry data. The link then
   only has to carry `TimeoutScale` bytes per timeout: the lowest
   bandwidth assumed is `TimeoutScale / timeout`, about 51 KiB/s with the
   default 256 KiB and a 5s timeout. The extra fixed timeout leaves room
   for up to `TimeoutScale` bytes still queued ahead of a pipelined
   request. v1.7.3 gave every batch the fixed timeout, so a batch larger
   than the link carries in one timeout failed on every attempt and the
   follower never caught up. InstallSnapshot is unchanged; it still uses
   `timeout * (Size / TimeoutScale)`, at least the fixed timeout.
   The deadline is not capped, since a cap would fail large batches on a
   slow link again; it is `timeout * (1 + batch bytes / TimeoutScale)`,
   about 9 minutes for 64 entries of 426 KB each at 5s. A request without
   entries, such as a heartbeat, keeps the fixed timeout, and so does
   every request when `TimeoutScale` is not positive (v1.7.3 divided by
   it only for InstallSnapshot, which still does).
   Guarded by `TestNetworkTransport_AppendEntriesScaledTimeout` (its
   `pipeline-buffered` case, whose writes return before the batch has
   crossed, guards the response deadline),
   `TestNetworkTransport_AppendEntriesScaledTimeoutValue` and
   `TestRaft_AppendEntriesScaledTimeoutCatchUp`
   (`slow_link_test.go`, added locally).

4. AppendEntries predecessor below the snapshot (`raft.go`,
   `appendEntries`). When `PrevLogEntry` is below the index of the last
   snapshot, the follower treats it as matching and skips batch entries up
   to that index. The snapshot holds only committed entries, which by
   Leader Completeness and Log Matching the leader's log holds unchanged.
   v1.7.3 looked the predecessor up in the compacted log and rejected the
   batch, so a leader that resent after a lost response, or to a follower
   that had installed a later snapshot, stepped back one index per round
   until it sent a snapshot instead.
   Such a request checks nothing past its own last entry, so the commit
   index is bounded by what the request covers: a follower commits up to
   `min(LeaderCommitIndex, PrevLogEntry + len(Entries))`, as in the Raft
   paper, and only if that is above its commit index. v1.7.3 used the
   follower's own last index in place of the request's, which commits
   whatever the follower holds past the request, checked or not. That is
   safe only while every accepted request reaches past any stale tail;
   a request below the snapshot need not, and a follower that installed a
   snapshot can keep a deposed leader's entries past it. The bound applies
   to every AppendEntries, wherever its predecessor lies, so no request
   commits past what it covers.
   Guarded by `TestRaft_AppendEntriesSnapshotBoundaryBelowSnapshot`,
   `TestRaft_AppendEntriesSnapshotBoundaryCommitIndex` and
   `TestRaft_AppendEntriesSnapshotBoundaryValidation`
   (`snapshot_boundary_test.go`, added locally).

## Files not carried over

Only the Go package, its tests, `go.mod`, `go.sum` and `LICENSE` are kept.
The upstream `.github`, `bench`, `docs`, `CHANGELOG.md`, `Makefile`,
`README.md`, `membership.md`, `tag.sh` and dotfiles are omitted; this file
is local and replaces the upstream `README.md`.

## Running the tests

The root `./...` pattern does not enter this nested module. Run it through
the replacement with the application's dependency versions, from the
repository root:

```
./scripts/test-raft.sh -race -count=1 -timeout 30m
./scripts/test-raft.sh -race -count=1 -run '^(TestRaft_AppendEntriesSnapshotBoundary|TestCommitment_|TestNetworkTransport_AppendEntriesScaledTimeout|TestRaft_AppendEntriesScaledTimeout)'
```

The second command is the local change regression step in
`.github/workflows/test.yml`.
