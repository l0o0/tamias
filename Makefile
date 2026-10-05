.PHONY: setup test check build cli run
setup:
	GOTOOLCHAIN=local go mod download
	npm --prefix frontend ci
test:
	GOTOOLCHAIN=local go test ./internal/... ./cmd/tami
	npm --prefix frontend run typecheck
check:
	GOTOOLCHAIN=local go test -race ./internal/... ./cmd/tami
	GOTOOLCHAIN=local go vet ./internal/... ./cmd/tami
	npm --prefix frontend run build
build:
	sh scripts/build-desktop.sh
cli:
	sh scripts/build-cli.sh
run: build
	open bin/tamias.app
