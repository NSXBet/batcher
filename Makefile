.PHONY: all clean

all: clean test coverage lint

clean:
	@rm -rf ./build
	@rm -rf ./dist

ensure-build:
	@mkdir -p ./build

test: unit

unit:
	@echo "Running unit tests..."
	@go test -v ./...

bench: bench-announce bench-run

bench-announce:
	@echo "Running benchmarks..."

bench-run:
	@go test -bench=. ./...

coverage: coverage-run coverage-report

coverage-run:
	@echo "Running coverage tests..."
	@go test -coverprofile=coverage.out ./...

coverage-report:
	@cat coverage.out | grep -v "test/" > coverage-filtered.out
	@go tool cover -func=coverage-filtered.out

coverage-report-ci: coverage-run
	@cat coverage.out | grep -v "test/" | grep -v "main.go" > coverage.txt

lint:
	@docker run \
	  -e LOG_LEVEL=DEBUG \
	  -e RUN_LOCAL=true \
	  -e DEFAULT_BRANCH=main \
      -e VALIDATE_GO=false \
      -e VALIDATE_JSCPD=false \
	  -v "${PWD}:/tmp/lint"  \
	  ghcr.io/super-linter/super-linter:latest
	@if [ $$? -ne 0 ]; then \
		echo "😞 Linting failed! Check the logs above for reasons."; \
		exit 1; \
	else \
		echo "🏆 Linting successful!"; \
	fi

release:
	@echo "Testing the release process..."
	@goreleaser release --snapshot --clean
