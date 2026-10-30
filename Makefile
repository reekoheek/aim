.PHONY: build test coverage coverage-html clean

COVERAGE_FILE := coverage.out
COVERAGE_HTML := coverage.html

# Default base version used when the repo has no tags yet.
BASE_VERSION := v0.1.0

# Version rules:
#   - exactly on a tag        -> {tag}            e.g. v0.1.0
#   - past a tag (any branch) -> {basetag}+{rev}  e.g. v0.1.0+54f7b14
#   - no tag at all           -> v0.1.0+{rev}     e.g. v0.1.0+54f7b14
VERSION := $(shell \
	if tag=$$(git describe --tags --exact-match 2>/dev/null); then \
		echo $$tag; \
	elif base=$$(git describe --tags --abbrev=0 2>/dev/null); then \
		echo $$base+$$(git rev-parse --short HEAD 2>/dev/null || echo unknown); \
	else \
		echo $(BASE_VERSION)+$$(git rev-parse --short HEAD 2>/dev/null || echo unknown); \
	fi)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/aim ./cmd/aim

test:
	go vet ./... && go test ./...

coverage:
	go test -coverprofile=$(COVERAGE_FILE) ./...
	go tool cover -func=$(COVERAGE_FILE)

coverage-html: coverage
	go tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)

clean:
	rm -f bin/aim $(COVERAGE_FILE) $(COVERAGE_HTML)
