# Whatsmeow D1 Adapter Makefile

# Variables
GO := go
BINARY_NAME := whatsmeow-d1-example
MDTEST_BINARY_NAME := whatsmeow-d1-mdtest
EXAMPLE_DIR := ./cmd/example
MDTEST_DIR := ./cmd/mdtest
BUILD_DIR := ./build
MAIN_PACKAGE := ./cmd/example
MDTEST_PACKAGE := ./cmd/mdtest
LIBRARY_PACKAGES := ./adapter/d1 ./internal/db

# Default target
.PHONY: all
all: clean build-lib build-example

# Build the library (compile check without creating binaries)
.PHONY: build-lib
build-lib:
	@echo "Building D1 adapter library..."
	$(GO) build $(LIBRARY_PACKAGES)
	@echo "Library compiled successfully"

# Build all packages (library + example + mdtest)
.PHONY: build
build: build-lib build-example build-mdtest

# Build the example binary
.PHONY: build-example
build-example:
	@echo "Building example binary..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PACKAGE)
	@echo "Binary built: $(BUILD_DIR)/$(BINARY_NAME)"

# Build the mdtest binary
.PHONY: build-mdtest
build-mdtest:
	@echo "Building mdtest binary..."
	@mkdir -p $(BUILD_DIR)
	cd $(MDTEST_DIR) && $(GO) mod tidy && $(GO) build -o ../../$(BUILD_DIR)/$(MDTEST_BINARY_NAME)
	@echo "Binary built: $(BUILD_DIR)/$(MDTEST_BINARY_NAME)"

# Build and run the example
.PHONY: run
run: build-example
	@echo "Running example..."
	@if [ -z "$(D1_ACCOUNT_ID)" ] || [ -z "$(D1_DATABASE_ID)" ] || [ -z "$(D1_API_TOKEN)" ]; then \
		echo "Error: D1_ACCOUNT_ID, D1_DATABASE_ID, and D1_API_TOKEN are all required"; \
		echo "Usage:"; \
		echo "  make run D1_ACCOUNT_ID=your-account D1_DATABASE_ID=your-db D1_API_TOKEN=your-token"; \
		exit 1; \
	fi
	D1_API_TOKEN="$(D1_API_TOKEN)" D1_ACCOUNT_ID="$(D1_ACCOUNT_ID)" D1_DATABASE_ID="$(D1_DATABASE_ID)" $(BUILD_DIR)/$(BINARY_NAME)

# Run directly with go run (useful for development)
.PHONY: dev
dev:
	@echo "Running example with go run..."
	@if [ -z "$(D1_ACCOUNT_ID)" ] || [ -z "$(D1_DATABASE_ID)" ] || [ -z "$(D1_API_TOKEN)" ]; then \
		echo "Error: D1_ACCOUNT_ID, D1_DATABASE_ID, and D1_API_TOKEN are all required"; \
		echo "Usage:"; \
		echo "  make dev D1_ACCOUNT_ID=your-account D1_DATABASE_ID=your-db D1_API_TOKEN=your-token"; \
		exit 1; \
	fi
	D1_API_TOKEN="$(D1_API_TOKEN)" D1_ACCOUNT_ID="$(D1_ACCOUNT_ID)" D1_DATABASE_ID="$(D1_DATABASE_ID)" $(GO) run $(MAIN_PACKAGE)

# Build and run mdtest
.PHONY: run-mdtest
run-mdtest: build-mdtest
	@echo "Running mdtest..."
	@if [ -z "$(D1_ACCOUNT_ID)" ] || [ -z "$(D1_DATABASE_ID)" ] || [ -z "$(D1_API_TOKEN)" ]; then \
		echo "Error: D1_ACCOUNT_ID, D1_DATABASE_ID, and D1_API_TOKEN are all required"; \
		echo "Usage:"; \
		echo "  make run-mdtest D1_ACCOUNT_ID=your-account D1_DATABASE_ID=your-db D1_API_TOKEN=your-token"; \
		exit 1; \
	fi
	D1_API_TOKEN="$(D1_API_TOKEN)" D1_ACCOUNT_ID="$(D1_ACCOUNT_ID)" D1_DATABASE_ID="$(D1_DATABASE_ID)" $(BUILD_DIR)/$(MDTEST_BINARY_NAME)

# Run mdtest directly with go run (useful for development)
.PHONY: dev-mdtest
dev-mdtest:
	@echo "Running mdtest with go run..."
	@if [ -z "$(D1_ACCOUNT_ID)" ] || [ -z "$(D1_DATABASE_ID)" ] || [ -z "$(D1_API_TOKEN)" ]; then \
		echo "Error: D1_ACCOUNT_ID, D1_DATABASE_ID, and D1_API_TOKEN are all required"; \
		echo "Usage:"; \
		echo "  make dev-mdtest D1_ACCOUNT_ID=your-account D1_DATABASE_ID=your-db D1_API_TOKEN=your-token"; \
		exit 1; \
	fi
	cd $(MDTEST_DIR) && D1_API_TOKEN="$(D1_API_TOKEN)" D1_ACCOUNT_ID="$(D1_ACCOUNT_ID)" D1_DATABASE_ID="$(D1_DATABASE_ID)" $(GO) run .

# Clean build artifacts
.PHONY: clean
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@echo "Clean completed"

# Download dependencies
.PHONY: deps
deps:
	@echo "Downloading dependencies..."
	$(GO) mod download
	$(GO) mod tidy
	@echo "Dependencies updated"

# Format code
.PHONY: fmt
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...
	@echo "Code formatted"

# Test the library packages
.PHONY: test-lib
test-lib:
	@echo "Running library tests..."
	$(GO) test $(LIBRARY_PACKAGES)
	@echo "Library tests completed"

# Run all tests
.PHONY: test
test:
	@echo "Running all tests..."
	$(GO) test ./...
	@echo "All tests completed"

# Run tests with coverage
.PHONY: test-coverage
test-coverage:
	@echo "Running tests with coverage..."
	$(GO) test -cover ./...
	@echo "Coverage tests completed"

# Run tests with verbose output
.PHONY: test-verbose
test-verbose:
	@echo "Running tests with verbose output..."
	$(GO) test -v ./...
	@echo "Verbose tests completed"

# Run linter (requires golangci-lint)
.PHONY: lint
lint:
	@echo "Running linter..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found. Install it with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		exit 1; \
	fi
	@echo "Linting completed"

# Build for multiple platforms
.PHONY: build-all
build-all: clean
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	@echo "Building for Linux AMD64..."
	GOOS=linux GOARCH=amd64 $(GO) build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(MAIN_PACKAGE)
	@echo "Building for Linux ARM64..."
	GOOS=linux GOARCH=arm64 $(GO) build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(MAIN_PACKAGE)
	@echo "Building for Darwin AMD64..."
	GOOS=darwin GOARCH=amd64 $(GO) build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(MAIN_PACKAGE)
	@echo "Building for Darwin ARM64..."
	GOOS=darwin GOARCH=arm64 $(GO) build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(MAIN_PACKAGE)
	@echo "Building for Windows AMD64..."
	GOOS=windows GOARCH=amd64 $(GO) build -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe $(MAIN_PACKAGE)
	@echo "Cross-platform builds completed"

# Create release archive
.PHONY: release
release: build-all
	@echo "Creating release archives..."
	@cd $(BUILD_DIR) && \
	for binary in $(BINARY_NAME)-*; do \
		if [[ "$$binary" == *".exe" ]]; then \
			zip "$${binary%.*}.zip" "$$binary"; \
		else \
			tar -czf "$$binary.tar.gz" "$$binary"; \
		fi; \
	done
	@echo "Release archives created in $(BUILD_DIR)/"

# Install the binary to GOPATH/bin
.PHONY: install
install:
	@echo "Installing binary..."
	$(GO) install $(MAIN_PACKAGE)
	@echo "Binary installed to $(shell go env GOPATH)/bin/$(BINARY_NAME)"

# Development setup
.PHONY: setup
setup: deps
	@echo "Setting up development environment..."
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "Installing golangci-lint..."; \
		$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
	fi
	@echo "Development setup completed"

# Verify library interfaces (check if library compiles and implements required interfaces)
.PHONY: verify
verify: build-lib
	@echo "Verifying library interfaces..."
	$(GO) vet $(LIBRARY_PACKAGES)
	@echo "Library verification completed"

# Check library code quality
.PHONY: check-lib
check-lib: fmt verify test-lib
	@echo "Library checks passed!"

# Check if everything is ready (library + example)
.PHONY: check
check: check-lib lint test
	@echo "All checks passed!"

# Show help
.PHONY: help
help:
	@echo "Whatsmeow D1 Adapter - Available targets:"
	@echo ""
	@echo "Library targets:"
	@echo "  build-lib     Build/compile the D1 adapter library"
	@echo "  test-lib      Run tests for library packages only"
	@echo "  verify        Verify library interfaces and run go vet"
	@echo "  check-lib     Run all library quality checks"
	@echo ""
	@echo "Example targets:"
	@echo "  build-example Build the example binary"
	@echo "  run           Build and run the example (requires D1 config)"
	@echo "  dev           Run example with go run (requires D1 config)"
	@echo ""
	@echo "MDTest targets:"
	@echo "  build-mdtest  Build the mdtest binary (full WhatsApp test client)"
	@echo "  run-mdtest    Build and run mdtest (requires D1 config)"
	@echo "  dev-mdtest    Run mdtest with go run (requires D1 config)"
	@echo ""
	@echo "General targets:"
	@echo "  build         Build library, example, and mdtest"
	@echo "  clean         Clean build artifacts"
	@echo "  deps          Download and tidy dependencies"
	@echo "  fmt           Format code"
	@echo "  test          Run all tests"
	@echo "  test-coverage Run tests with coverage"
	@echo "  test-verbose  Run tests with verbose output"
	@echo "  lint          Run linter (requires golangci-lint)"
	@echo "  build-all     Build example for multiple platforms"
	@echo "  release       Create release archives"
	@echo "  install       Install example binary to GOPATH/bin"
	@echo "  setup         Setup development environment"
	@echo "  check         Run all quality checks (library + example)"
	@echo "  help          Show this help message"
	@echo ""
	@echo "Examples:"
	@echo "  make build-lib                              # Build the library"
	@echo "  make check-lib                              # Verify library quality"
	@echo ""
	@echo "  # For Cloudflare D1:"
	@echo "  make run D1_ACCOUNT_ID=abc D1_DATABASE_ID=xyz D1_API_TOKEN=token"
	@echo "  make dev D1_ACCOUNT_ID=abc D1_DATABASE_ID=xyz D1_API_TOKEN=token"
	@echo ""
	@echo "  # Full WhatsApp client with D1:"
	@echo "  make run-mdtest D1_ACCOUNT_ID=abc D1_DATABASE_ID=xyz D1_API_TOKEN=token"
	@echo "  make dev-mdtest D1_ACCOUNT_ID=abc D1_DATABASE_ID=xyz D1_API_TOKEN=token"
	@echo ""
	@echo "  make build                                  # Build everything"
	@echo "  make test                                   # Test everything"

# Default help when no target is specified
.DEFAULT_GOAL := help
.PHONY: code-vuln code-go-versions
code-vuln: ## Scan all Go modules and their standard library with the declared toolchain
	@result=0; for module in . cmd/mdtest; do \
		bash scripts/code-vuln.sh "$$module" || result=1; \
	done; exit $$result

code-go-versions: ## Check module, CI and Docker compiler versions
	python3 scripts/check-go-versions.py
