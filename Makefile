.PHONY: build test vet check

build:
	go build ./cmd/mailpit-graphapi

test:
	go test ./...

vet:
	go vet ./...

check: test vet build
