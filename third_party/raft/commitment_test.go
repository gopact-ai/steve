// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"testing"
)

func makeConfiguration(voters []string) Configuration {
	var configuration Configuration
	for _, voter := range voters {
		configuration.Servers = append(configuration.Servers, Server{
			Suffrage: Voter,
			Address:  ServerAddress(voter + "addr"),
			ID:       ServerID(voter),
		})
	}
	return configuration
}

// Returns a slice of server names of size n.
func voters(n int) Configuration {
	if n > 7 {
		panic("only up to 7 servers implemented")
	}
	return makeConfiguration([]string{"s1", "s2", "s3", "s4", "s5", "s6", "s7"}[:n])
}

// Tests setVoters() keeps matchIndexes where possible.
func TestCommitment_setVoters(t *testing.T) {
	commitCh := make(chan struct{}, 1)
	c := newCommitment(commitCh, makeConfiguration([]string{"a", "b", "c"}), 0)
	c.match("a", 10)
	c.match("b", 20)
	c.match("c", 30)
	// commitIndex: 20
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}
	c.setConfiguration(makeConfiguration([]string{"c", "d", "e"}))
	// c: 30, d: 0, e: 0
	c.match("e", 40)
	if c.getCommitIndex() != 30 {
		t.Fatalf("expected 30 entries committed, found %d",
			c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}
}

// Tests match() being called with smaller index than before.
func TestCommitment_match_max(t *testing.T) {
	commitCh := make(chan struct{}, 1)
	c := newCommitment(commitCh, voters(5), 4)

	c.match("s1", 8)
	c.match("s2", 8)
	c.match("s2", 1)
	c.match("s3", 8)

	if c.getCommitIndex() != 8 {
		t.Fatalf("calling match with an earlier index should be ignored")
	}
}

// Tests match() being called with non-voters.
func TestCommitment_match_nonVoting(t *testing.T) {
	commitCh := make(chan struct{}, 1)
	c := newCommitment(commitCh, voters(5), 4)

	c.match("s1", 8)
	c.match("s2", 8)
	c.match("s3", 8)

	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}

	c.match("s90", 10)
	c.match("s91", 10)
	c.match("s92", 10)

	if c.getCommitIndex() != 8 {
		t.Fatalf("non-voting servers shouldn't be able to commit")
	}
	if drainNotifyCh(commitCh) {
		t.Fatalf("unexpected commit notify")
	}
}

// Tests recalculate() algorithm.
func TestCommitment_recalculate(t *testing.T) {
	commitCh := make(chan struct{}, 1)
	c := newCommitment(commitCh, voters(5), 0)

	c.match("s1", 30)
	c.match("s2", 20)

	if c.getCommitIndex() != 0 {
		t.Fatalf("shouldn't commit after two of five servers")
	}
	if drainNotifyCh(commitCh) {
		t.Fatalf("unexpected commit notify")
	}

	c.match("s3", 10)
	if c.getCommitIndex() != 10 {
		t.Fatalf("expected 10 entries committed, found %d",
			c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}
	c.match("s4", 15)
	if c.getCommitIndex() != 15 {
		t.Fatalf("expected 15 entries committed, found %d",
			c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}

	c.setConfiguration(voters(3))
	// s1: 30, s2: 20, s3: 10
	if c.getCommitIndex() != 20 {
		t.Fatalf("expected 20 entries committed, found %d",
			c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}

	c.setConfiguration(voters(4))
	// s1: 30, s2: 20, s3: 10, s4: 0
	c.match("s2", 25)
	if c.getCommitIndex() != 20 {
		t.Fatalf("expected 20 entries committed, found %d",
			c.getCommitIndex())
	}
	if drainNotifyCh(commitCh) {
		t.Fatalf("unexpected commit notify")
	}
	c.match("s4", 23)
	if c.getCommitIndex() != 23 {
		t.Fatalf("expected 23 entries committed, found %d",
			c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}
}

// Tests recalculate() respecting startIndex.
func TestCommitment_recalculate_startIndex(t *testing.T) {
	commitCh := make(chan struct{}, 1)
	c := newCommitment(commitCh, voters(5), 4)

	c.match("s1", 3)
	c.match("s2", 3)
	c.match("s3", 3)

	if c.getCommitIndex() != 0 {
		t.Fatalf("can't commit until startIndex is replicated to a quorum")
	}
	if drainNotifyCh(commitCh) {
		t.Fatalf("unexpected commit notify")
	}

	c.match("s1", 4)
	c.match("s2", 4)
	c.match("s3", 4)

	if c.getCommitIndex() != 4 {
		t.Fatalf("should be able to commit startIndex once replicated to a quorum")
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}
}

// With no voting members in the cluster, the most sane behavior is probably
// to not mark anything committed.
func TestCommitment_noVoterSanity(t *testing.T) {
	commitCh := make(chan struct{}, 1)
	c := newCommitment(commitCh, makeConfiguration([]string{}), 4)
	c.match("s1", 10)
	c.setConfiguration(makeConfiguration([]string{}))
	c.match("s1", 10)
	if c.getCommitIndex() != 0 {
		t.Fatalf("no voting servers: shouldn't be able to commit")
	}
	if drainNotifyCh(commitCh) {
		t.Fatalf("unexpected commit notify")
	}

	// add a voter so we can commit something and then remove it
	c.setConfiguration(voters(1))
	c.match("s1", 10)
	if c.getCommitIndex() != 10 {
		t.Fatalf("expected 10 entries committed, found %d",
			c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}

	c.setConfiguration(makeConfiguration([]string{}))
	c.match("s1", 20)
	if c.getCommitIndex() != 10 {
		t.Fatalf("expected 10 entries committed, found %d",
			c.getCommitIndex())
	}
	if drainNotifyCh(commitCh) {
		t.Fatalf("unexpected commit notify")
	}
}

// Single voter commits immediately.
func TestCommitment_singleVoter(t *testing.T) {
	commitCh := make(chan struct{}, 1)
	c := newCommitment(commitCh, voters(1), 4)
	c.match("s1", 10)
	if c.getCommitIndex() != 10 {
		t.Fatalf("expected 10 entries committed, found %d",
			c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}
	c.setConfiguration(voters(1))
	if drainNotifyCh(commitCh) {
		t.Fatalf("unexpected commit notify")
	}
	c.match("s1", 12)
	if c.getCommitIndex() != 12 {
		t.Fatalf("expected 12 entries committed, found %d",
			c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}
}

// Tests that a server's match survives its promotion to voter. The leader's
// appendConfigurationEntry dispatches the promoting entry, which wakes
// replication, before it calls setConfiguration, so the promoted server can
// acknowledge that entry before it votes.
func TestCommitment_promotedServerKeepsEarlierMatch(t *testing.T) {
	for _, suffrage := range []ServerSuffrage{Nonvoter, Staging} {
		t.Run(suffrage.String(), func(t *testing.T) {
			commitCh := make(chan struct{}, 1)
			configuration := makeConfiguration([]string{"s1", "s2", "s3"})
			configuration.Servers = append(configuration.Servers, Server{Suffrage: suffrage, ID: "s4", Address: "s4addr"})
			c := newCommitment(commitCh, configuration, 4)
			c.match("s1", 5)
			c.match("s2", 5)
			c.match("s3", 5)
			if c.getCommitIndex() != 5 || !drainNotifyCh(commitCh) {
				t.Fatalf("expected 5 entries committed, found %d", c.getCommitIndex())
			}

			// Entry 6 promotes s4. s1 is the leader; s3 no longer answers.
			c.match("s1", 6)
			c.match("s4", 6)
			if c.getCommitIndex() != 5 {
				t.Fatalf("a %v server's match must not commit, found %d", suffrage, c.getCommitIndex())
			}
			if drainNotifyCh(commitCh) {
				t.Fatalf("unexpected commit notify")
			}
			c.setConfiguration(voters(4))
			c.match("s2", 6)
			if c.getCommitIndex() != 6 {
				t.Fatalf("expected 6 entries committed with s1, s2 and s4, found %d", c.getCommitIndex())
			}
			if !drainNotifyCh(commitCh) {
				t.Fatalf("expected commit notify")
			}
		})
	}
}

// Tests that a demoted voter's match stops counting but is kept, so it counts
// again if the server is promoted later in the same term.
func TestCommitment_demotedVoterKeepsMatch(t *testing.T) {
	commitCh := make(chan struct{}, 1)
	c := newCommitment(commitCh, voters(3), 4)
	c.match("s1", 5)
	c.match("s2", 5)
	if c.getCommitIndex() != 5 || !drainNotifyCh(commitCh) {
		t.Fatalf("expected 5 entries committed, found %d", c.getCommitIndex())
	}

	// Demote s2. s1 is the leader; s3 no longer answers.
	demoted := voters(3)
	demoted.Servers[1].Suffrage = Nonvoter
	c.setConfiguration(demoted)
	c.match("s1", 6)
	c.match("s2", 6)
	if c.getCommitIndex() != 5 {
		t.Fatalf("a demoted server's match must not commit, found %d", c.getCommitIndex())
	}
	if drainNotifyCh(commitCh) {
		t.Fatalf("unexpected commit notify")
	}

	c.setConfiguration(voters(3))
	if c.getCommitIndex() != 6 {
		t.Fatalf("expected 6 entries committed with s1 and s2, found %d", c.getCommitIndex())
	}
	if !drainNotifyCh(commitCh) {
		t.Fatalf("expected commit notify")
	}
}
