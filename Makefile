VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
BINARIES := pogfarm clipper
PLATFORMS := linux/amd64 linux/arm64

.PHONY: build cross test vet fmt fmt-check clean

## build: compile both binaries for this machine into bin/
build:
	@for b in $(BINARIES); do \
		go build -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; \
	done

## cross: compile both binaries for every platform the images target, into dist/
cross:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		for b in $(BINARIES); do \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				go build -ldflags "$(LDFLAGS)" -o dist/$$os-$$arch/$$b ./cmd/$$b || exit 1; \
		done; \
	done

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

## fmt-check: fail if any file is not gofmt-formatted
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "not formatted:"; echo "$$out"; exit 1; fi

clean:
	rm -rf bin dist
