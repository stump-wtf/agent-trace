.PHONY: test lint check

test:
	go test ./...

lint:
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*')); \
	if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "WARNING: golangci-lint not installed — skipping locally, but CI still runs it."; \
		echo "         install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2"; \
	fi

check: lint test
