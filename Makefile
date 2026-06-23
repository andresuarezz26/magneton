VERSION ?= dev
LDFLAGS := -s -w -X github.com/andresuarezz26/magneton/cmd.version=$(VERSION)

.PHONY: build install test vet lint snapshot clean

build: ## build the `magneton` binary
	go build -ldflags "$(LDFLAGS)" -o magneton .

install: ## build and install to ~/.local/bin/magneton
	go build -ldflags "$(LDFLAGS)" -o $(HOME)/.local/bin/magneton .

test: ## run tests
	go test ./...

vet: ## go vet
	go vet ./...

snapshot: ## local release build (no publish) — requires goreleaser
	goreleaser release --snapshot --clean

clean:
	rm -rf magneton dist
