#!/bin/bash
set -euo pipefail

# Root ./... does not enter nested modules. Test the local Raft replacement
# explicitly, using the application's dependency versions.
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
exec go test -mod=readonly github.com/hashicorp/raft "$@"
