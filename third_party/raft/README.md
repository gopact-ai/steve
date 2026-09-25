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
   timeout for every `TimeoutScale` bytes of entry data, so the link only
   has to carry `TimeoutScale` bytes per timeout, as for InstallSnapshot.
   v1.7.3 gave every batch the fixed timeout, so a batch larger than the
   link carries in one timeout failed on every attempt and the follower
   never caught up. A request without entries, such as a heartbeat, keeps
   the fixed timeout.
   Guarded by `TestNetworkTransport_AppendEntriesScaledTimeout` and
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
   Guarded by `TestRaft_AppendEntriesSnapshotBoundaryBelowSnapshot` and
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
