# torrentd build entry points (§12).

GO      ?= go
BINARY  ?= torrentd

.PHONY: all build web test vet depcheck gate clean

all: build

# build compiles the static binary. It builds the web UI first when web/
# holds a frontend project; otherwise the committed placeholder
# web/dist/index.html is embedded.
build: web
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BINARY) ./cmd/torrentd

web:
	@if [ -f web/package.json ]; then \
		cd web && npm ci && npm run build; \
	else \
		echo "web/package.json not found: embedding the placeholder web/dist/index.html"; \
	fi

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

depcheck:
	$(GO) run ./tools/depcheck .

# gate runs the milestone 1 acceptance checks.
gate:
	harness/factory gate --ms 1

clean:
	rm -f $(BINARY)
