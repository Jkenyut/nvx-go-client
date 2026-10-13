-include .env
export

.PHONY: test lint clean help 

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Targets:'
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

test: ## Run all unit tests
	go test -v ./...

test-cover: ## Run tests with coverage
	go test -v -cover ./...

lint: ## Run golangci-lint
	golangci-lint run --fix ./... 
	
clean: ## Clean build artifacts
	go clean
	rm -f coverage.out
