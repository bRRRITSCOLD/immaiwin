MODULE := github.com/bRRRITSCOLD/immaiwin-go
CMDS    := api ui worker
BINDIR  := bin

.PHONY: setup build test lint clean api ui start-worker list-workers dev-ui docker-compose-up docker-compose-down certs

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
	mkcert -cert-file .private/certs/localhost.pem -key-file .private/certs/localhost-key.pem 127.0.0.1

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

