.PHONY: build build-freebsd-amd64 build-freebsd-arm64 test test-v test-cover bench clean fmt lint version sqllogictest sqllogictest-basic sqllogictest-download build-sqllogictest install uninstall

PREFIX ?= /usr/local

GIT_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
GIT_DATE := $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/danfragoso/pizzasql-next/pkg/version.Version=$(GIT_VERSION) -X github.com/danfragoso/pizzasql-next/pkg/version.Commit=$(GIT_COMMIT) -X github.com/danfragoso/pizzasql-next/pkg/version.Date=$(GIT_DATE)

# Build the project
build:
	go build -ldflags "$(LDFLAGS)" -o ./bin/pizzasql ./main.go

# Install pizzasql to PREFIX/bin (default: /usr/local/bin)
install: build
	sudo install -d $(DESTDIR)$(PREFIX)/bin
	sudo install -m 755 ./bin/pizzasql $(DESTDIR)$(PREFIX)/bin/pizzasql
	@echo "Installed to $(DESTDIR)$(PREFIX)/bin/pizzasql"

# Remove installed binary
uninstall:
	sudo rm -f $(DESTDIR)$(PREFIX)/bin/pizzasql
	@echo "Removed $(DESTDIR)$(PREFIX)/bin/pizzasql"

build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o ./bin/pizzasql-linux-amd64 ./main.go

build-freebsd-amd64:
	GOOS=freebsd GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o ./bin/pizzasql-freebsd-amd64 ./main.go

build-freebsd-arm64:
	GOOS=freebsd GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o ./bin/pizzasql-freebsd-arm64 ./main.go

# Print the version that a tag build would embed (git describe, commit-hash fallback).
version:
	@echo "$(GIT_VERSION)"

# Run all tests
test:
	go test ./...

# Run tests with verbose output
test-v:
	go test -v ./...

# Run tests with coverage
test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run benchmarks
bench:
	go test -bench=. -benchmem ./...

# Run lexer tests only
test-lexer:
	go test -v ./pkg/lexer/...

# Run parser tests only
test-parser:
	go test -v ./pkg/parser/...

# Format code
fmt:
	go fmt ./...

# Run linter (requires golangci-lint)
lint:
	golangci-lint run

# Clean build artifacts
clean:
	rm -f pizzasql coverage.out coverage.html

# Run tests with race detection
test-race:
	go test -race ./...

# Quick test for development
quick:
	go test -short ./...

# ── sqllogictest ──────────────────────────────────────────────────────────────

# Run the sqllogictest suite against a running PizzaSQL server (default: localhost:8080).
# Set URL= to point at a different server, e.g. make sqllogictest URL=http://host:9090
sqllogictest:
	go run ./cmd/sqllogictest -url $(or $(URL),http://localhost:8080) -dir testdata/sqllogictest -log sqllogictest-failures.log

# Run only the built-in smoke test.
sqllogictest-basic:
	go run ./cmd/sqllogictest -url $(or $(URL),http://localhost:8080) -file testdata/sqllogictest/basic.test

# Download the official SQLite sqllogictest corpus into testdata/sqllogictest/.
# Requires curl and tar. Files are not committed to the repo.
sqllogictest-download:
	@echo "Downloading SQLite sqllogictest corpus..."
	mkdir -p testdata/sqllogictest
	mkdir -p /tmp/sqllogictest-dl
	curl -fsSL https://github.com/gregrahn/sqllogictest/archive/refs/heads/master.tar.gz \
	  | tar xz -C /tmp/sqllogictest-dl
	find /tmp/sqllogictest-dl -name '*.test' | while read f; do \
	  rel=$$(echo "$$f" | sed 's|.*/test/||'); \
	  dir=testdata/sqllogictest/$$(dirname "$$rel"); \
	  mkdir -p "$$dir"; \
	  cp "$$f" "$$dir/"; \
	done
	rm -rf /tmp/sqllogictest-dl
	@echo "Done. $$(find testdata/sqllogictest -name '*.test' | wc -l | tr -d ' ') test files in testdata/sqllogictest/"

# Build the sqllogictest runner binary.
build-sqllogictest:
	go build -o ./bin/sqllogictest ./cmd/sqllogictest
