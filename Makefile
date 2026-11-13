.PHONY: build windows test check integration

build:
	CGO_ENABLED=0 go build -o bin/overlaybd-go ./cmd/overlaybd-go

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o bin/overlaybd-go-windows-amd64.exe ./cmd/overlaybd-go

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...

integration: build
	sudo env OVERLAYBD_GO_BIN=$(CURDIR)/bin/overlaybd-go python3 scripts/ublk-smoke.py
