.PHONY: all clean

all: clean test

clean:
	@rm -f build

test: unit

unit:
	@echo "Running unit tests..."
	@go test -v ./...

bench:
	@echo "Running benchmarks..."
	@go test -bench=. ./...

coverage:
	@echo "Running coverage tests..."
	@go test -coverprofile=coverage.out ./...
	@cat coverage.out | grep -v "test/" > coverage-filtered.out
	@go tool cover -func=coverage-filtered.out
