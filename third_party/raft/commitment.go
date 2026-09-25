// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"sort"
	"sync"
)

// Commitment is used to advance the leader's commit index. The leader and
// replication goroutines report in newly written entries with match(), and
// this notifies on commitCh when the commit index has advanced.
type commitment struct {
	// protects matchIndexes, voters and commitIndex
	sync.Mutex
	// notified when commitIndex increases
	commitCh chan struct{}
	// server ID to log index: the server stores up through this log entry.
	// Every server in the configuration is tracked, nonvoters included, so
	// a match reported before a server's promotion counts once it votes.
	matchIndexes map[ServerID]uint64
	// the servers whose matchIndexes count toward commitIndex
	voters map[ServerID]struct{}
	// a quorum stores up through this log entry. monotonically increases.
	commitIndex uint64
	// the first index of this leader's term: this needs to be replicated to a
	// majority of the cluster before this leader may mark anything committed
	// (per Raft's commitment rule)
	startIndex uint64
}

// newCommitment returns a commitment struct that notifies the provided
// channel when log entries have been committed. A new commitment struct is
// created each time this server becomes leader for a particular term.
// 'configuration' is the servers in the cluster.
// 'startIndex' is the first index created in this term (see
// its description above).
func newCommitment(commitCh chan struct{}, configuration Configuration, startIndex uint64) *commitment {
	matchIndexes := make(map[ServerID]uint64)
	voters := make(map[ServerID]struct{})
	for _, server := range configuration.Servers {
		matchIndexes[server.ID] = 0
		if server.Suffrage == Voter {
			voters[server.ID] = struct{}{}
		}
	}
	return &commitment{
		commitCh:     commitCh,
		matchIndexes: matchIndexes,
		voters:       voters,
		commitIndex:  0,
		startIndex:   startIndex,
	}
}

// Called when a new cluster membership configuration is created: it will be
// used to determine commitment from now on. 'configuration' is the servers in
// the cluster.
func (c *commitment) setConfiguration(configuration Configuration) {
	c.Lock()
	defer c.Unlock()
	oldMatchIndexes := c.matchIndexes
	c.matchIndexes = make(map[ServerID]uint64)
	c.voters = make(map[ServerID]struct{})
	for _, server := range configuration.Servers {
		c.matchIndexes[server.ID] = oldMatchIndexes[server.ID] // defaults to 0
		if server.Suffrage == Voter {
			c.voters[server.ID] = struct{}{}
		}
	}
	c.recalculate()
}

// Called by leader after commitCh is notified
func (c *commitment) getCommitIndex() uint64 {
	c.Lock()
	defer c.Unlock()
	return c.commitIndex
}

// Match is called once a server completes writing entries to disk: either the
// leader has written the new entry or a follower has replied to an
// AppendEntries RPC. The given server's disk agrees with this server's log up
// through the given index.
func (c *commitment) match(server ServerID, matchIndex uint64) {
	c.Lock()
	defer c.Unlock()
	if prev, inConfiguration := c.matchIndexes[server]; inConfiguration && matchIndex > prev {
		c.matchIndexes[server] = matchIndex
		// Only voters count toward commitIndex, so another server's match
		// cannot move it.
		if _, voter := c.voters[server]; voter {
			c.recalculate()
		}
	}
}

// Internal helper to calculate new commitIndex from the voters' matchIndexes.
// Must be called with lock held.
func (c *commitment) recalculate() {
	if len(c.voters) == 0 {
		return
	}

	matched := make([]uint64, 0, len(c.voters))
	for id := range c.voters {
		matched = append(matched, c.matchIndexes[id])
	}
	sort.Sort(uint64Slice(matched))
	quorumMatchIndex := matched[(len(matched)-1)/2]

	if quorumMatchIndex > c.commitIndex && quorumMatchIndex >= c.startIndex {
		c.commitIndex = quorumMatchIndex
		asyncNotifyCh(c.commitCh)
	}
}
