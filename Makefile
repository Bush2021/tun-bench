.PHONY: all test fmt lint

all:
	go build -trimpath -ldflags=-checklinkname=0 .

test:
	go test -race -ldflags=-checklinkname=0 ./...

fmt:
	golangci-lint fmt

lint:
	GOOS=linux GOARCH=amd64 golangci-lint run ./...
	GOOS=windows GOARCH=amd64 golangci-lint run ./...
	GOOS=darwin GOARCH=arm64 golangci-lint run ./...
