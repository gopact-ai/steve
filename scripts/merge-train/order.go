package main

import (
	"fmt"
	"sort"
	"strings"
)

// candidate is what ordering needs to know about one listed pull request.
type candidate struct {
	n     int
	index int    // position in the owner's list
	head  string // branch name
	fork  bool   // head lives in a fork, so it cannot be another PR's base
	base  string
	files []string
	ready bool // up to date with its base and green: merging it costs no CI run
}

// overlaps maps each pull request to the listed ones sharing a file with it,
// with the shared files.
func overlaps(cs []candidate) map[int]map[int][]string {
	owners := map[string][]int{}
	for _, c := range cs {
		for _, f := range c.files {
			owners[f] = append(owners[f], c.n)
		}
	}
	out := map[int]map[int][]string{}
	for f, ns := range owners {
		for _, a := range ns {
			for _, b := range ns {
				if a == b {
					continue
				}
				if out[a] == nil {
					out[a] = map[int][]string{}
				}
				out[a][b] = append(out[a][b], f)
			}
		}
	}
	for _, m := range out {
		for b := range m {
			sort.Strings(m[b])
		}
	}
	return out
}

// parents maps a stacked pull request to the listed one whose branch is its
// base. Only heads in the base repository can be bases: a fork's "master"
// is not upstream "master".
func parents(cs []candidate) map[int]int {
	byHead := map[string]int{}
	for _, c := range cs {
		if !c.fork {
			byHead[c.head] = c.n
		}
	}
	out := map[int]int{}
	for _, c := range cs {
		if p, ok := byHead[c.base]; ok && p != c.n {
			out[c.n] = p
		}
	}
	return out
}

// order returns the merge order:
//   - a stacked pull request follows the listed one it is based on;
//   - pull requests sharing files keep the owner's order among themselves,
//     so a later conflict is reported against the one the owner put first;
//   - pull requests that share no file with any other go first, in list order;
//   - one that is already up to date and green goes to the very front when
//     nothing must precede it: merging it costs no CI run, while every later
//     merge needs one update and one CI run whatever the order.
func order(cs []candidate) ([]int, error) {
	byN := map[int]candidate{}
	for _, c := range cs {
		byN[c.n] = c
	}
	over, par := overlaps(cs), parents(cs)
	ancestor := func(a, b int) bool { // a is below b in a stack
		for seen := map[int]bool{}; !seen[b]; {
			seen[b] = true
			p, ok := par[b]
			if !ok {
				return false
			}
			if p == a {
				return true
			}
			b = p
		}
		return false
	}
	after := map[int][]int{} // edges: key before each value
	indeg := map[int]int{}
	edge := func(a, b int) { after[a] = append(after[a], b); indeg[b]++ }
	for child, p := range par {
		edge(p, child)
	}
	for a, m := range over {
		for b := range m {
			if ancestor(a, b) || ancestor(b, a) {
				continue // the stack already orders them
			}
			if byN[a].index < byN[b].index {
				edge(a, b)
			}
		}
	}
	less := func(a, b int) bool {
		oa, ob := len(over[a]) > 0, len(over[b]) > 0
		if oa != ob {
			return !oa
		}
		return byN[a].index < byN[b].index
	}
	var ready []int
	for _, c := range cs {
		if indeg[c.n] == 0 {
			ready = append(ready, c.n)
		}
	}
	var out []int
	first := true
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		pick := 0
		if first {
			for i, n := range ready {
				if byN[n].ready {
					pick = i
					break
				}
			}
			first = false
		}
		n := ready[pick]
		ready = append(ready[:pick], ready[pick+1:]...)
		out = append(out, n)
		for _, b := range after[n] {
			if indeg[b]--; indeg[b] == 0 {
				ready = append(ready, b)
			}
		}
	}
	if len(out) != len(cs) {
		var stuck []string
		for _, c := range cs {
			if indeg[c.n] > 0 {
				stuck = append(stuck, fmt.Sprintf("#%d", c.n))
			}
		}
		return nil, fmt.Errorf("stacked pull requests form a cycle: %s", strings.Join(stuck, " "))
	}
	return out, nil
}
