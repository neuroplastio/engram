GO ?= go

# What a build calls itself. The commit is the identity; the version is for
# people: the commit's date, CalVer-style, and that it came from main.
COMMIT   := $(shell git rev-parse HEAD)
EPOCH    := $(shell git show -s --format=%ct HEAD)
VERSION  ?= $(shell date -u -d @$(EPOCH) +%y.%m.%d)-dev.$(shell git rev-parse --short=7 HEAD)
LDFLAGS  := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# Two modules: the root, and enboot/ — its own so that a program can embed it
# without taking on anything else. name:module-directory.
BINARIES  := engram:. enboot:enboot
MODULES   := . enboot
# linux/arm is 32-bit ARMv6 (GOARM below): the one build runs on every
# Raspberry Pi. Every Linux build is static (no cgo), so it also runs on
# Android under Termux, which has no glibc.
PLATFORMS := linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64
GOARM     := 6

.PHONY: all check build test fmt dist clean

all: check

## check — the gate: formatted, vetted, tested.
check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	@for m in $(MODULES); do \
		$(GO) -C $$m vet ./... && $(GO) -C $$m test ./... || exit 1; \
	done
	@echo "ok: all checks passed"

## build — both binaries, for this machine.
build:
	@for bm in $(BINARIES); do b=$${bm%:*}; m=$${bm#*:}; \
		$(GO) build -C $$m -trimpath -ldflags "$(LDFLAGS)" -o $(CURDIR)/bin/$$b ./cmd/$$b || exit 1; \
	done

## dist — release tarballs for every platform, into dist/.
##
## Byte-for-byte reproducible from a commit: no cgo, -trimpath, and an archive
## with fixed order, owner and times. The hashes of these files get signed, so
## "rebuild it and compare" should be something anyone can do.
dist:
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do os=$${p%/*}; arch=$${p#*/}; \
		for bm in $(BINARIES); do b=$${bm%:*}; m=$${bm#*:}; \
			stage=$(CURDIR)/dist/.stage/$${b}_$${os}_$${arch}; mkdir -p $$stage; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$(GOARM) $(GO) build -C $$m -trimpath -ldflags "$(LDFLAGS)" -o $$stage/$$b ./cmd/$$b || exit 1; \
			tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@$(EPOCH) -cf - -C $$stage $$b \
				| gzip -n > dist/$${b}_$${os}_$${arch}.tar.gz || exit 1; \
		done; \
	done
	@rm -rf dist/.stage
	@echo "$(VERSION) $(COMMIT)"; ls dist

test:
	@for m in $(MODULES); do $(GO) -C $$m test ./... || exit 1; done

fmt:
	gofmt -w .

clean:
	rm -rf bin dist
