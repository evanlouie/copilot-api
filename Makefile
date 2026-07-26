# Local mirror of the gates in .github/workflows/ci.yml.
# CI runs these same commands inline (one step per gate) so failures are easy to
# read in the Actions UI; this Makefile lets a developer run exactly the same
# checks locally. Keep the two in sync.

# Pinned to match the workflow. Bump both together.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.7.0

.DEFAULT_GOAL := ci
.PHONY: ci go-ci deno-ci fmt fmt-check build vet test test-short lint \
        deno-fmt-check deno-check deno-test

# Everything CI runs.
ci: go-ci deno-ci

go-ci: fmt-check build vet test lint

deno-ci: deno-fmt-check deno-check deno-test

# Rewrite Go sources in gofmt style.
fmt:
	gofmt -w .

# `gofmt -l` exits 0 even when it lists files, so check the output explicitly.
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Files are not gofmt-formatted. Run 'make fmt' to fix:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "All Go files are gofmt-clean."

build:
	go build ./...

vet:
	go vet ./...

# -count=1 defeats the test cache. Live tests skip unless COPILOT_API_LIVE_TESTS=1.
test:
	go test ./... -race -count=1

# Fast inner loop. The storage tests are fsync-bound (F_FULLFSYNC costs ~5ms per
# sync on macOS), so their soak loops dominate a full run; -short scales those
# loops down while still running every assertion. Use this while iterating, and
# `make test` before pushing.
test-short:
	go test ./... -count=1 -short

lint:
	go run $(STATICCHECK) ./...

deno-fmt-check:
	deno fmt --check

# Covers both files in the suite; `deno task test:ai-sdk` only checks mcp_server.ts.
deno-check:
	deno check tests/ai-sdk-deno

# Skips 22 cases unless COPILOT_API_AI_SDK_DENO_TESTS=1 and a server is running.
deno-test:
	deno task test:ai-sdk
