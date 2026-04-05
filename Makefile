BINARY_NAME := collei-agent
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w -X github.com/collei-monitor/collei-agent/internal/core.Version=$(VERSION)

.PHONY: all build clean test lint

all: build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) .

# 交叉编译目标
build-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME)-linux-amd64 .

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME)-linux-arm64 .

build-darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME)-darwin-amd64 .

build-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME)-darwin-arm64 .

build-windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME)-windows-amd64.exe .

build-windows-arm64:
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME)-windows-arm64.exe .

build-all: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64 build-windows-arm64

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME)-*

tidy:
	go mod tidy
