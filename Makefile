BINARY := gbsnap
PREFIX ?= /usr/local
VERSION := $(shell cat VERSION)

# Release builds are stamped with the version and stripped, and carry no
# absolute paths from the machine that built them.
LDFLAGS := -s -w -X github.com/greendrake/gbsnap/internal/cli.version=v$(VERSION)
BUILDFLAGS := -trimpath -ldflags '$(LDFLAGS)'

# The platforms a release publishes a binary for.
PLATFORMS := amd64 arm64

.PHONY: build test test-integration lint fmt install dist clean

build:
	go build $(BUILDFLAGS) -o $(BINARY) .

# Unit tests: no root, no btrfs.
test:
	go test ./...

# Integration tests: need the btrfs tools and root, or a passwordless sudo.
# They skip cleanly where those are missing.
test-integration:
	go test -tags integration -v ./integration/

# What CI gates on. Both build tag sets are vetted, or the integration tests
# would be free to rot.
lint:
	@unformatted=$$(gofmt -l .); \
	  if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go vet -tags integration ./...

fmt:
	gofmt -w .

install: build
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)

# The release artefacts. Static binaries: gbsnap shells out to btrfs and ssh and
# links nothing, so CGO buys it nothing and costs it portability.
dist:
	rm -rf dist && mkdir dist
	for arch in $(PLATFORMS); do \
	  GOOS=linux GOARCH=$$arch CGO_ENABLED=0 \
	    go build $(BUILDFLAGS) -o dist/$(BINARY)-linux-$$arch . || exit 1; \
	done
	cd dist && sha256sum $(BINARY)-linux-* > SHA256SUMS

clean:
	rm -rf $(BINARY) dist
