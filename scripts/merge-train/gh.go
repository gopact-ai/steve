package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// PR is what the train reads about a pull request.
type PR struct {
	Number           int    `json:"number"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	State            string `json:"state"` // OPEN, MERGED, CLOSED
	IsDraft          bool   `json:"isDraft"`
	MergeStateStatus string `json:"mergeStateStatus"` // BEHIND, BLOCKED, CLEAN, DIRTY, HAS_HOOKS, UNKNOWN, UNSTABLE
	ReviewDecision   string `json:"reviewDecision"`
	HeadRefName      string `json:"headRefName"`
	HeadRefOid       string `json:"headRefOid"`
	BaseRefName      string `json:"baseRefName"`
	// IsCrossRepository marks a head branch in a fork: its name says nothing
	// about branches of the base repository.
	IsCrossRepository bool `json:"isCrossRepository"`
}

// Check is one row of `gh pr checks --json`.
type Check struct {
	Name     string `json:"name"`
	Bucket   string `json:"bucket"` // pass, fail, pending, skipping, cancel
	State    string `json:"state"`
	Link     string `json:"link"`
	Workflow string `json:"workflow"`
}

// GH is every call the train makes. Only UpdateBranch and Merge change
// anything, and the train calls them only for listed pull requests.
type GH interface {
	View(n int) (PR, error)
	Checks(n int, required bool) ([]Check, error)
	UnresolvedThreads(n int) (int, error)
	Files(n int) ([]string, error)
	UpdateBranch(n int) error
	Merge(n int, head string) error
}

// cli runs the gh binary. exec, when set, replaces the process call (tests).
type cli struct {
	bin, repo string
	owner     string
	name      string
	exec      func(args []string) (stdout, stderr []byte, err error)
}

const prFields = "number,title,url,state,isDraft,mergeStateStatus,reviewDecision,headRefName,headRefOid,baseRefName,isCrossRepository"

func (c *cli) run(args ...string) ([]byte, error) {
	var out, errOut []byte
	var err error
	if c.exec != nil {
		out, errOut, err = c.exec(args)
	} else {
		cmd := exec.Command(c.bin, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err = cmd.Output()
		errOut = stderr.Bytes()
	}
	if err != nil {
		return out, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(errOut)))
	}
	return out, nil
}

// pr runs a `gh pr` subcommand against the configured repository.
func (c *cli) pr(args ...string) ([]byte, error) {
	if c.repo != "" {
		args = append(args, "--repo", c.repo)
	}
	return c.run(append([]string{"pr"}, args...)...)
}

func (c *cli) View(n int) (PR, error) {
	var pr PR
	out, err := c.pr("view", strconv.Itoa(n), "--json", prFields)
	if err != nil {
		return pr, err
	}
	return pr, json.Unmarshal(out, &pr)
}

// Checks tolerates gh's non-zero exit for failing or pending checks: the
// JSON on stdout is still the answer.
func (c *cli) Checks(n int, required bool) ([]Check, error) {
	args := []string{"checks", strconv.Itoa(n), "--json", "name,bucket,state,link,workflow"}
	if required {
		args = append(args, "--required")
	}
	out, err := c.pr(args...)
	var checks []Check
	if len(bytes.TrimSpace(out)) > 0 {
		if jerr := json.Unmarshal(out, &checks); jerr == nil {
			return checks, nil
		}
	}
	if err != nil && strings.Contains(err.Error(), "no ") && strings.Contains(err.Error(), "checks reported") {
		return nil, nil // none registered yet
	}
	if err == nil {
		err = fmt.Errorf("gh pr checks %d: unreadable output %q", n, out)
	}
	return nil, err
}

func (c *cli) repoID() error {
	if c.owner != "" {
		return nil
	}
	args := []string{"repo", "view", "--json", "owner,name"}
	if c.repo != "" {
		args = []string{"repo", "view", c.repo, "--json", "owner,name"}
	}
	out, err := c.run(args...)
	if err != nil {
		return err
	}
	var r struct {
		Owner struct{ Login string } `json:"owner"`
		Name  string                 `json:"name"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return err
	}
	c.owner, c.name = r.Owner.Login, r.Name
	return nil
}

const threadsQuery = `query($owner:String!,$name:String!,$n:Int!){repository(owner:$owner,name:$name){pullRequest(number:$n){reviewThreads(first:100){totalCount pageInfo{hasNextPage} nodes{isResolved}}}}}`

// UnresolvedThreads counts open review threads. More than one page is
// treated as unresolved rather than guessed.
func (c *cli) UnresolvedThreads(n int) (int, error) {
	if err := c.repoID(); err != nil {
		return 0, err
	}
	out, err := c.run("api", "graphql", "-f", "query="+threadsQuery, "-F", "owner="+c.owner, "-F", "name="+c.name, "-F", "n="+strconv.Itoa(n))
	if err != nil {
		return 0, err
	}
	var r struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						TotalCount int `json:"totalCount"`
						PageInfo   struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return 0, err
	}
	threads := r.Data.Repository.PullRequest.ReviewThreads
	open := 0
	for _, t := range threads.Nodes {
		if !t.IsResolved {
			open++
		}
	}
	if threads.PageInfo.HasNextPage {
		open += threads.TotalCount - len(threads.Nodes)
	}
	return open, nil
}

func (c *cli) Files(n int) ([]string, error) {
	out, err := c.pr("diff", strconv.Itoa(n), "--name-only")
	if err != nil {
		return nil, err
	}
	return diffNames(out), nil
}

// diffNames splits `gh pr diff --name-only` output on line breaks only:
// paths may contain spaces.
func diffNames(out []byte) []string {
	var files []string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

// updateArgs merges the base into the branch: a new commit on top, never a
// rebase or force-push of someone else's branch.
func updateArgs(n int) []string { return []string{"update-branch", strconv.Itoa(n)} }

// mergeArgs makes a merge commit of exactly the head that was checked.
// Never --admin (bypasses protection) and never --auto.
func mergeArgs(n int, head string) []string {
	return []string{"merge", strconv.Itoa(n), "--merge", "--match-head-commit", head}
}

func (c *cli) UpdateBranch(n int) error { _, err := c.pr(updateArgs(n)...); return err }

func (c *cli) Merge(n int, head string) error { _, err := c.pr(mergeArgs(n, head)...); return err }
