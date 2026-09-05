GO ?= go

.PHONY: build console test e2e e2e-fleet

# The console is a React app built into internal/readmodel/web/dist and
# embedded into the binary; rebuild it after touching web/console.
console:
	cd web/console && npm install --no-audit --no-fund && npm run build

build: 
	CGO_ENABLED=0 $(GO) build -o steve ./cmd/steve
	CGO_ENABLED=0 $(GO) build -o steve-node ./cmd/steve-node

test:
	$(GO) test -race ./...

e2e:
	STEVE_MESH_E2E=1 $(GO) test -count=1 -timeout 25m ./e2e/mesh/

# Uses an already-running hub. HUB / TOKEN override config.e2e.json.
e2e-fleet:
	@bash -lc '$(GO) run ./e2e/fleet'
