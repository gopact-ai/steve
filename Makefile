GO ?= go

.PHONY: build console test test-console e2e e2e-lab e2e-fleet e2e-autonomous

# The console is a React app built into internal/readmodel/web/dist and
# embedded into the binary; rebuild it after touching web/console.
console:
	cd web/console && npm ci --no-audit --no-fund && npm run build

build: 
	CGO_ENABLED=0 $(GO) build -o steve ./cmd/steve
	CGO_ENABLED=0 $(GO) build -o steve-node ./cmd/steve-node

test:
	$(GO) test -race ./cmd/... ./internal/... ./e2e/...

test-console:
	npm --prefix web/console run test:boundaries
	npm --prefix web/console run test:unit
	npm --prefix web/console run build
	npm --prefix web/console run test:ui
	npm --prefix web/console run test:architecture
	npm --prefix web/console run test:settings
	npm --prefix web/console run test:materials
	npm --prefix web/console run test:questions
	npm --prefix web/console run test:desktop
	npm --prefix web/console run test:ssh
	npm --prefix web/console run test:coordination
	npm --prefix web/console run test:node-agents
	npm --prefix web/console run test:plugins
	npm --prefix web/console run test:selection

.PHONY: desktop
desktop:
	./scripts/build-desktop.sh

# The three-host suite. Machines come from e2e/fleetlab: a container per
# node by default, or ones you name through STEVE_LAB_<NODE>_ADDR and
# friends when the run needs real agents and credentials.
e2e:
	STEVE_MESH_E2E=1 $(GO) test -count=1 -timeout 25m ./e2e/mesh/

# What the suite's machines are, on their own: separate filesystems and
# process tables, one address that both this process and the other nodes
# reach, and a node that can be stopped and started without losing work.
e2e-lab:
	$(GO) test -count=1 -timeout 10m ./e2e/fleetlab/

# Owns a disposable hub and Docker nodes; missing prerequisites fail the gate.
# Linux host + local Docker with a matching Go toolchain are required.
e2e-fleet:
	$(GO) run ./e2e/fleetlab/cmd/gate

# The coordinator is told only the goal: it must look the fleet up, split
# the work, place it by capability and report. The lab supplies kvtool,
# a deterministic ACP agent and a copy of the host Go toolchain on node-b.
e2e-autonomous:
	$(GO) run ./e2e/fleetlab/cmd/gate -scenario autonomous
