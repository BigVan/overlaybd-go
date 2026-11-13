.PHONY: build test check integration

build:
	CGO_ENABLED=0 go build -o bin/overlaybd-go ./cmd/overlaybd-go

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...

integration: build
	sudo env OVERLAYBD_GO_BIN=$(CURDIR)/bin/overlaybd-go python3 scripts/ublk-smoke.py
