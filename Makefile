MODULE := github.com/bRRRITSCOLD/immaiwin-go
CMDS    := api ui worker
BINDIR  := bin

.PHONY: setup build test lint clean api ui start-worker list-workers dev-ui docker-compose-up docker-compose-down certs sandbox-images sandbox-images-debug sandbox-images-push dev-teardown dev-teardown-sandbox dev-teardown-full dev-startup dev-startup-fresh

build:
	go build ./...

test:
	go run ./scripts/test/main.go

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
	  echo "==> building immaiwin/sandbox-$$img:$$tag (from $$dir)"; \
	  docker build -t $(REGISTRY)/immaiwin/sandbox-$$img:$$tag internal/sandbox/runtimes/$$dir || exit $$?; \
	done

sandbox-images-debug:
	@for pair in $(SANDBOX_DEBUG_PAIRS); do \
	  dir=$${pair%%:*}; rest=$${pair#*:}; img=$${rest%%:*}; tag=$${rest#*:}; \
	  echo "==> building immaiwin/sandbox-$$img:$$tag (from $$dir Dockerfile.debug)"; \
	  docker build -f internal/sandbox/runtimes/$$dir/Dockerfile.debug \
	    -t $(REGISTRY)/immaiwin/sandbox-$$img:$$tag internal/sandbox/runtimes/$$dir || exit $$?; \
	done

sandbox-images-push: sandbox-images sandbox-images-debug
	@for pair in $(SANDBOX_BASE_PAIRS) $(SANDBOX_DEBUG_PAIRS); do \
	  rest=$${pair#*:}; img=$${rest%%:*}; tag=$${rest#*:}; \
	  docker push $(REGISTRY)/immaiwin/sandbox-$$img:$$tag || exit $$?; \
	done

# --- Dev teardown ---
# Soft stop for the end of a session: brings down docker compose, stops k3s,
# stops the local registry container. State is preserved — restart with
# `systemctl start k3s`, `docker start registry`, `make docker-compose-up`.
SANDBOX_NS ?= immaiwin-sandbox
KUBECONFIG_K3S ?= /etc/rancher/k3s/k3s.yaml

dev-teardown: ## stop docker compose, k3s, and the local registry (state preserved)
	@echo "==> stopping docker compose stack"
	-@$(MAKE) --no-print-directory docker-compose-down
	@echo "==> stopping local registry container (if running)"
	-@docker stop registry >/dev/null 2>&1 || true
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

