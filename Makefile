BINARY   := rvfs
CMD_PATH := ./cmd/rvfs
BUILD_DIR := bin

.PHONY: all build test lint clean install

all: build

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY) $(CMD_PATH)

test:
	go test ./...

test-fuse:
	go test -v -timeout 120s ./internal/fuse/...

test-config:
	go test -v ./internal/config/...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR)
	go clean

install:
	go install $(CMD_PATH)

tidy:
	go mod tidy
