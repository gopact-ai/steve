#!/usr/bin/env python3
"""Split `go test -race` into balanced CI shards and prove nothing was dropped.

  shard.py go-plan N I [ALL]       Go packages for shard I of N (from `go list`)
  shard.py verify ALL SHARD...     fail unless the shards cover ALL exactly once
  shard.py go-weights LOG...       print go-weights.txt from `go test` (-json) output

Work is placed longest-first onto the least-loaded shard using the measured
seconds in go-weights.txt. Anything without a weight (a new package) then
goes to the lightest shard, so it is never skipped; `verify` checks the union
against the full list.
"""
import json
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
GO_PATTERNS = ["./cmd/...", "./internal/...", "./e2e/..."]


def load_weights(name):
    weights = {}
    with open(os.path.join(HERE, name)) as f:
        for line in f:
            line = line.rstrip("\n")
            if line and not line.startswith("#"):
                seconds, key = line.split("\t", 1)
                weights[key] = float(seconds)
    return weights


def assign(items, weights, shards):
    """Longest-processing-time-first; unweighted items then fill the lightest shard."""
    loads = [0.0] * shards
    bins = [[] for _ in range(shards)]
    known = sorted((i for i in items if i in weights), key=lambda i: (-weights[i], i))
    unknown = [i for i in items if i not in weights]
    for item in known + unknown:
        k = min(range(shards), key=lambda s: (loads[s], len(bins[s]), s))
        bins[k].append(item)
        loads[k] += weights.get(item, 0.0)
    return bins, loads


def go_packages():
    return subprocess.check_output(["go", "list", *GO_PATTERNS], cwd=ROOT, text=True).split()




def plan(items, weights, shards, index, all_out):
    bins, loads = assign(items, weights, shards)
    for s, (b, load) in enumerate(zip(bins, loads)):
        print(f"shard {s}/{shards}: {len(b)} items, {load:.0f}s measured", file=sys.stderr)
    if all_out:
        with open(all_out, "w") as f:
            f.write("".join(i + "\n" for i in items))
    print("\n".join(bins[index]))


def read_lines(path):
    with open(path) as f:
        return [l.rstrip("\n") for l in f if l.strip()]




def verify(all_path, shard_paths):
    want = read_lines(all_path)
    got = [i for p in shard_paths for i in read_lines(p)]
    problems = [("missing", sorted(set(want) - set(got))),
                ("ran twice", sorted({i for i in got if got.count(i) > 1})),
                ("unexpected", sorted(set(got) - set(want)))]
    for label, items in problems:
        for i in items:
            print(f"{label}: {i}")
    if not want or any(items for _, items in problems):
        sys.exit("shards do not cover the full list exactly once")
    print(f"{len(want)} items covered exactly once by {len(shard_paths)} shards")


def go_weights(paths):
    seconds = {}
    for p in paths:
        with open(p) as f:
            for line in f:
                # `go test -json` events, or the plain `ok  <pkg>  <n>s` summary lines.
                plain = re.search(r"\b(?:ok|FAIL)\s+(\S+)\s+([\d.]+)s\b", line)
                try:
                    e = json.loads(line)
                except ValueError:
                    if plain:
                        seconds[plain.group(1)] = float(plain.group(2))
                    continue
                if e.get("Test") is None and e.get("Action") in ("pass", "fail", "skip") and "Elapsed" in e:
                    seconds[e["Package"]] = e["Elapsed"]
    print_weights(seconds)




def print_weights(seconds):
    for key, s in sorted(seconds.items(), key=lambda x: (-x[1], x[0])):
        print(f"{s:.1f}\t{key}")


def main(argv):
    cmd = argv[1] if len(argv) > 1 else ""
    if cmd == "go-plan":
        plan(go_packages(), load_weights("go-weights.txt"), int(argv[2]), int(argv[3]), argv[4] if len(argv) > 4 else None)
    elif cmd == "verify":
        verify(argv[2], argv[3:])
    elif cmd == "go-weights":
        go_weights(argv[2:])
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main(sys.argv)
