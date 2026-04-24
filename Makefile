MODULE := github.com/bRRRITSCOLD/immaiwin-go
CMDS    := api ui worker
BINDIR  := bin

.PHONY: setup build test lint clean run-api run-ui run-worker list-workers run-dev-ui

setup:
	go install -modfile=tools/go.mod tool
	lefthook install

build:
	go build ./...

test:
	go run ./scripts/test/main.go

lint:
	golangci-lint run

clean:
	rm -rf $(BINDIR)

$(BINDIR)/%: cmd/%/main.go
	@mkdir -p $(BINDIR)
	go build -o $@ ./cmd/$*/

run-api:
	go run ./cmd/api

run-ui:
	go run ./cmd/ui

run-dev-ui:
	go run ./cmd/ui -dev

run-worker: ## usage: make run-worker NAME=example
	go run ./cmd/worker -name $(NAME)

list-workers:
	go run ./cmd/worker -list
