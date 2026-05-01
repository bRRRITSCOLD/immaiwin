MODULE := github.com/bRRRITSCOLD/burrow
CMDS    := api ui worker
BINDIR  := bin

.PHONY: setup build test test-unit test-integration test-e2e lint clean api ui start-worker list-workers dev-ui docker-compose-up docker-compose-down certs sandbox-images sandbox-images-debug sandbox-images-push dev-teardown dev-teardown-sandbox dev-teardown-full dev-startup dev-startup-fresh

build:
	go build ./...

# Run only unit tests (files matching *_test.go without tier build tags).
# Fast — no service deps. Default target for quick iteration.
test-unit:
	go test -race -count=1 ./...

# Run only integration tests (files matching *_integration_test.go gated
# by `//go:build integration`). Requires the docker compose stack to be
# UP — suites fail loud (no skip path) if Mongo/Redis unreachable.
test-integration:
	go test -tags=integration -race -count=1 ./...

# Run only e2e tests (files matching *_e2e_test.go gated by `//go:build
# e2e`). Requires the full stack — API + UI + workers + services.
test-e2e:
	go test -tags=e2e -race -count=1 ./...

# Run every tier in sequence. CI uses this. Local devs typically run
# `make test-unit` while iterating; `make test` before pushing.
test: test-unit test-integration test-e2e

lint:
	golangci-lint run

setup:
	go install -modfile=tools/go.mod tool
	lefthook install
	cd internal/ui && pnpm install

certs:
	mkcert -install
	mkcert -cert-file .private/certs/localhost.pem -key-file .private/certs/localhost-key.pem localhost

docker-compose-up:
	go run ./scripts/docker-compose/main.go up

docker-compose-down:
	go run ./scripts/docker-compose/main.go down
api:
	go run ./cmd/api

ui:
	go run ./cmd/ui

dev-ui:
	go run ./cmd/ui -dev

list-workers:
	@go run ./cmd/worker -list

worker: ## usage: make run-worker NAME=example
	go run ./cmd/worker -name $(NAME)

clean:
	rm -rf $(BINDIR)

$(BINDIR)/%: cmd/%/main.go
	@mkdir -p $(BINDIR)
	go build -o $@ ./cmd/$*/

# --- Sandbox images ---
# Tag pairs (dir:image:tag) must match internal/sandbox/types.go
# {Image,DebugImage}ForLanguage so the api process and registry agree.
# REGISTRY is the local registry that k3s pulls from. Override with REGISTRY=...
SANDBOX_BASE_PAIRS := python:python:3.12 javascript:node:20 golang:go:1.22 rust:rust:1.86 php:php:8.3
SANDBOX_DEBUG_PAIRS := python:python-debug:3.12 javascript:node-debug:20
REGISTRY ?= localhost:5000

sandbox-images:
	@for pair in $(SANDBOX_BASE_PAIRS); do \
	  dir=$${pair%%:*}; rest=$${pair#*:}; img=$${rest%%:*}; tag=$${rest#*:}; \
	  echo "==> building burrow/sandbox-$$img:$$tag (from $$dir)"; \
	  docker build -t $(REGISTRY)/burrow/sandbox-$$img:$$tag internal/sandbox/runtimes/$$dir || exit $$?; \
	done

sandbox-images-debug:
	@for pair in $(SANDBOX_DEBUG_PAIRS); do \
	  dir=$${pair%%:*}; rest=$${pair#*:}; img=$${rest%%:*}; tag=$${rest#*:}; \
	  echo "==> building burrow/sandbox-$$img:$$tag (from $$dir Dockerfile.debug)"; \
	  docker build -f internal/sandbox/runtimes/$$dir/Dockerfile.debug \
	    -t $(REGISTRY)/burrow/sandbox-$$img:$$tag internal/sandbox/runtimes/$$dir || exit $$?; \
	done

sandbox-images-push: sandbox-images sandbox-images-debug
	@for pair in $(SANDBOX_BASE_PAIRS) $(SANDBOX_DEBUG_PAIRS); do \
	  rest=$${pair#*:}; img=$${rest%%:*}; tag=$${rest#*:}; \
	  docker push $(REGISTRY)/burrow/sandbox-$$img:$$tag || exit $$?; \
	done

# --- Dev teardown ---
# Soft stop for the end of a session: brings down docker compose, stops k3s,
# stops the local registry container. State is preserved — restart with
# `systemctl start k3s`, `docker start registry`, `make docker-compose-up`.
SANDBOX_NS ?= burrow-sandbox
KUBECONFIG_K3S ?= /etc/rancher/k3s/k3s.yaml

dev-teardown: ## stop docker compose, k3s, local registry, reap orphans by name + kill orphan docker-proxy procs
	@echo "==> stopping docker compose stack"
	-@$(MAKE) --no-print-directory docker-compose-down
	@echo "==> reaping by-name orphans (legacy compose project names + cross-context drift)"
	@# `docker compose down` only reaps containers labelled with the
	@# CURRENT compose project name. Renaming the project (immaiwin →
	@# burrow) leaves the old containers labelled with the old name,
	@# invisible to compose. Reaping by name regex catches them so a
	@# port-already-allocated boot loop can't form on the next dev-up.
	-@for prefix in burrow- immaiwin-; do \
	  ids=$$(docker ps -aq --filter "name=$$prefix" 2>/dev/null); \
	  if [ -n "$$ids" ]; then \
	    echo "    removing $$prefix* containers ($$(echo $$ids | wc -w) found)"; \
	    docker rm -f $$ids >/dev/null 2>&1 || true; \
	  fi; \
	done
	@echo "==> stopping local registry container (if running)"
	-@docker stop registry >/dev/null 2>&1 || true
	@docker rm -f registry >/dev/null 2>&1 || true
	@echo "==> killing orphan docker-proxy procs holding our dev ports"
	@# When dockerd kills a container ungracefully (compose race, daemon
	@# restart mid-rm), the userland docker-proxy children get reparented
	@# to PID 1 and keep holding the host port. Container is gone but the
	@# next dev-up gets `Bind for 0.0.0.0:<port> failed: port is already
	@# allocated`. Reap by host-port; ports must match the published
	@# ports in docker-compose.yml + scripts/registry.
	-@sudo pkill -f 'docker-proxy.*-host-port (6379|5672|27017|5000|1025|8025|15672|15692)' 2>/dev/null || true
	@echo "==> stopping k3s service"
	-@if systemctl list-unit-files k3s.service >/dev/null 2>&1; then \
	  sudo systemctl stop k3s; \
	else \
	  echo "k3s.service not installed; skipping"; \
	fi
	@echo "==> dev teardown complete"

dev-teardown-sandbox: ## delete all pods in the sandbox namespace (cluster stays up)
	@echo "==> deleting pods in $(SANDBOX_NS)"
	-@sudo KUBECONFIG=$(KUBECONFIG_K3S) kubectl delete pods -n $(SANDBOX_NS) --all --ignore-not-found

dev-teardown-full: dev-teardown ## DESTRUCTIVE: also uninstall k3s + remove registry container/data
	@echo
	@echo "WARNING: this removes /var/lib/rancher/k3s, the registry container, and gVisor."
	@echo "Press Ctrl-C within 5s to abort."
	@sleep 5
	@echo "==> uninstalling k3s"
	-@if [ -x /usr/local/bin/k3s-uninstall.sh ]; then sudo /usr/local/bin/k3s-uninstall.sh; \
	else echo "/usr/local/bin/k3s-uninstall.sh not found; skipping"; fi
	@echo "==> removing local registry container"
	-@docker rm -f registry >/dev/null 2>&1 || true
	@echo "==> removing gVisor (runsc)"
	-@if dpkg -l runsc >/dev/null 2>&1; then sudo apt-get remove -y runsc; \
	else echo "runsc not installed via apt; skipping"; fi
	@echo "==> full teardown complete"

# --- Dev startup ---
# Inverse of dev-teardown: starts k3s, starts the local registry, brings up
# docker compose. Idempotent — safe to re-run.
dev-startup: ## start k3s, the local registry, and docker compose (inverse of dev-teardown)
	@echo "==> starting k3s service"
	-@if systemctl list-unit-files k3s.service >/dev/null 2>&1; then \
	  if systemctl is-active --quiet k3s; then \
	    echo "k3s already running"; \
	  else \
	    sudo systemctl start k3s; \
	  fi; \
	else \
	  echo "k3s.service not installed; skipping (run 'sudo ./examples/k3s/setup.sh' to install)"; \
	fi
	@echo "==> starting local registry container"
	-@if docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^registry$$'; then \
	  echo "registry already running"; \
	elif docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q '^registry$$'; then \
	  docker start registry >/dev/null; \
	else \
	  echo "registry container missing — creating"; \
	  docker run -d --restart=always -p 5000:5000 --name registry registry:2 >/dev/null; \
	fi
	@echo "==> starting docker compose stack"
	-@$(MAKE) --no-print-directory docker-compose-up
	@echo "==> dev startup complete"
	@echo
	@echo "Next: 'make api' (in another terminal: 'make dev-ui')."

dev-startup-fresh: ## DESTRUCTIVE-leaning: full first-time bring-up (k3s setup script + sandbox images + docker compose)
	@echo "==> running k3s setup script (idempotent)"
	@if [ -x examples/k3s/setup.sh ]; then \
	  sudo ./examples/k3s/setup.sh; \
	else \
	  echo "examples/k3s/setup.sh missing or not executable; skipping"; \
	fi
	@echo "==> building + pushing sandbox images"
	@$(MAKE) --no-print-directory sandbox-images-push
	@echo "==> bringing up docker compose stack"
	@$(MAKE) --no-print-directory docker-compose-up
	@echo "==> fresh dev environment ready"
	@echo
	@echo "Next: 'make api' (in another terminal: 'make dev-ui')."

