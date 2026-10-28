GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race vet check clean runner-images

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ ./cmd/racoonci

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

# Everything that must pass before merging.
check: vet race

clean:
	rm -rf dist

# Refresh the embedded snapshot of what GitHub's Ubuntu runner images preinstall (needs network).
runner-images:
	$(GO) run ./cmd/racoonci runner-images update internal/runnerimages/snapshot
