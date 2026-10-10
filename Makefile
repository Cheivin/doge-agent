# doge build helpers. Stamps the version and current git hash into the
# binary so `doge --version` reports which commit it was built from, e.g.
# "0.4.0 (10f8bca)" or "0.4.0 (10f8bca-dirty)" for a dirty working tree.
#
# Mesh: the P2P layer is compiled in only with `-tags mesh` (default builds
# stay libp2p-free). Use the build-mesh* targets for mesh-enabled binaries.
VERSION   ?= 0.11.0
GIT_HASH  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GIT_DIRTY := $(shell git status --porcelain 2>/dev/null | grep -q . && echo -dirty || echo "")
LDFLAGS   := -s -w -X 'main.version=$(VERSION)' -X 'main.gitCommit=$(GIT_HASH)$(GIT_DIRTY)'

.PHONY: build build-mesh build-linux build-mesh-linux build-darwin desktop desktop-mesh desktop-app test test-mesh clean

build: ## build for the current platform
	go build -trimpath -o dist/doge -ldflags "$(LDFLAGS)" ./cmd/doge

build-mesh: ## build with P2P mesh support (-tags mesh)
	go build -trimpath -tags mesh -o dist/doge-mesh -ldflags "$(LDFLAGS)" ./cmd/doge

build-linux: ## cross-compile for linux/amd64
	GOOS=linux GOARCH=amd64 go build -trimpath -o dist/doge-linux-amd64 -ldflags "$(LDFLAGS)" ./cmd/doge

build-mesh-linux: ## cross-compile mesh-enabled binary for linux/amd64
	GOOS=linux GOARCH=amd64 go build -trimpath -tags mesh -o dist/doge-mesh-linux-amd64 -ldflags "$(LDFLAGS)" ./cmd/doge

build-darwin: ## cross-compile for darwin/amd64
	GOOS=darwin GOARCH=amd64 go build -trimpath -o dist/doge-darwin-amd64 -ldflags "$(LDFLAGS)" ./cmd/doge

# Desktop client (wails v3, separate module in desktop/). CGO is required on
# macOS: the window links Cocoa/WebKit.
desktop: ## build the desktop client (mesh compiled in, runtime switchable)
	cd desktop && CGO_ENABLED=1 go build -trimpath -tags mesh -o ../dist/doge-desktop -ldflags "$(LDFLAGS)" .

desktop-mesh: ## alias of desktop (mesh is compiled in and switchable at runtime)
	cd desktop && CGO_ENABLED=1 go build -trimpath -tags mesh -o ../dist/doge-desktop-mesh -ldflags "$(LDFLAGS)" .

desktop-app: ## package dist/doge-desktop as a macOS .app (requires the wails3 CLI)
	@command -v wails3 >/dev/null 2>&1 || { \
		echo "wails3 CLI not found. Install it with:"; \
		echo "  go install github.com/wailsapp/wails/v3/cmd/wails3@latest"; \
		exit 1; }
	cd desktop && wails3 package -b ../dist/doge-desktop

test: ## run the default (non-mesh) test suite
	go test ./...

test-mesh: ## run the mesh test suite (-tags mesh)
	go test -tags mesh ./internal/mesh/...

clean:
	rm -f doge doge-mesh doge-linux-amd64 doge-mesh-linux-amd64 dist/doge-darwin-amd64 dist/doge-desktop dist/doge-desktop-mesh
