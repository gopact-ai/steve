GO ?= go

.PHONY: build console test test-console gate gate-full

# The console is a React app built into internal/readmodel/web/dist and
# embedded into the binary; rebuild it after touching web/console.
console:
	cd web/console && npm ci --no-audit --no-fund && npm run build

build:
	CGO_ENABLED=0 $(GO) build -o steve ./cmd/steve
	CGO_ENABLED=0 $(GO) build -o steve-node ./cmd/steve-node

test:
	$(GO) test -race -timeout 30m ./cmd/... ./internal/... ./scripts/...

# The local loop: gofmt, go vet and go test -race on the packages a change
# reaches (changed packages and their reverse dependencies), plus the
# console scripts whose imports reach it. Falls back to gate-full when the
# reach is not computed. GATE_BASE overrides origin/master; ARGS=-n prints
# the plan only.
gate:
	$(GO) run ./scripts/gate $(ARGS)

# The required CI jobs (test, macos), run locally;
# jobs this host cannot run are reported and left to CI.
gate-full:
	bash scripts/gate/full.sh

test-console:
	npm --prefix providers/mail ci --ignore-scripts --no-audit --no-fund
	npm --prefix web/console run test:boundaries
	npm --prefix web/console run test:unit
	npm --prefix web/console run build
	npm --prefix web/console run test:styles
	npm --prefix web/console run test:performance
	npm --prefix web/console run test:ui
	npm --prefix web/console run test:mail
	npm --prefix web/console run test:task-completion
	npm --prefix web/console run test:architecture
	npm --prefix web/console run test:appearance
	npm --prefix web/console run test:settings
	npm --prefix web/console run test:materials
	npm --prefix web/console run test:questions
	npm --prefix web/console run test:durable-drafts
	npm --prefix web/console run test:streaming
	npm --prefix web/console run test:desktop
	npm --prefix web/console run test:ssh
	npm --prefix web/console run test:coordination
	npm --prefix web/console run test:node-agents
	npm --prefix web/console run test:machine-restart
	npm --prefix web/console run test:plugins
	npm --prefix web/console run test:selection
	npm --prefix web/console run test:projects
	npm --prefix web/console run test:delete
	npm --prefix web/console run test:conflicts
	npm --prefix web/console run test:close-layers
