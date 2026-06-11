VERSION ?= dev
LDFLAGS := -s -w -X github.com/droidpilot/droidpilot/cmd.version=$(VERSION)

.PHONY: build install test vet lint snapshot clean

build: ## build the `agent` binary
	go build -ldflags "$(LDFLAGS)" -o agent .

install: ## install to $GOBIN
	go install -ldflags "$(LDFLAGS)" .

test: ## run tests
	go test ./...

vet: ## go vet
	go vet ./...

snapshot: ## local release build (no publish) — requires goreleaser
	goreleaser release --snapshot --clean

clean:
	rm -rf agent dist
