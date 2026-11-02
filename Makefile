GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build runner-binary test race vet check clean runner-images

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ ./cmd/raccoonci ./cmd/raccoonci-runner ./cmd/raccoonci-server

# The binary the InstaBox runner snapshot installs: static, so it runs on any
# template. Build it with a real VERSION; `raccoonci runner-snapshot` refuses "dev".
runner-binary:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/raccoonci-linux-amd64 ./cmd/raccoonci

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
	$(GO) run ./cmd/raccoonci runner-images update pkg/runnerimages/snapshot
