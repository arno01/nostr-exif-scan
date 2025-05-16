# nostr-exif-scan Makefile

APP_NAME = nostr-exif-scan

.PHONY: build run clean help

## Default: build the binary
all: build

## Build the CLI binary
build:
	go build -o $(APP_NAME) main.go

## Run the scanner with example args (override via CLI)
run: build
	./$(APP_NAME) --npub $$NPUB --threads 8 --limit 5000 -v

## Clean up built binary
clean:
	rm -f $(APP_NAME)

## Show help
help:
	@echo "Available targets:"
	@echo "  build     Build the CLI tool"
	@echo "  run       Run the scanner with example args"
	@echo "             Usage: make run NPUB=npub1yourpubkey"
	@echo "  clean     Remove built binaries"
