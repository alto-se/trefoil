ADDR     ?= :8080
DATA_DIR ?= $(CURDIR)/.data
CONFIG   ?= examples/basic/config.yml
URL      ?= http://localhost$(ADDR)
SETTINGS ?= $(CURDIR)/backend.yml
AIR      ?= go run github.com/air-verse/air@v1.62.0

.PHONY: dev seed build test test-modules verify cli clean help

## dev: run the API locally, restarting on file changes (Ctrl+C to stop); uses SETTINGS if it exists
dev:
ifneq ($(wildcard $(SETTINGS)),)
	$(AIR) -c .air.toml -- -settings $(SETTINGS)
else
	ADDR=$(ADDR) DATA_DIR=$(DATA_DIR) MODULES_DIR=$(CURDIR)/modules $(AIR) -c .air.toml
endif

## seed: package CONFIG and upload it to the running dev server
seed:
	trefoil config sync $(CONFIG) --server $(URL)

## build: compile the API binary to bin/api
build:
	cd api && go build -o ../bin/api ./cmd/api

## test: run the API tests
test:
	cd api && go test ./...

## test-modules: build each shared module's image and run its tests.py in it (MODULES="core:os code:go" to pick)
test-modules:
	cd api && go run ./cmd/moduletest -modules $(CURDIR)/modules $(MODULES)

## verify: sync every example to the dev server and build all its agents' images (EXAMPLES="examples/go" to pick)
verify:
	TREFOIL_SERVER=$(URL) scripts/verify $(EXAMPLES)

## cli: install the trefoil CLI (editable)
cli:
	uv tool install --editable ./cli --force

## clean: remove build output and local dev data
clean:
	rm -rf bin .air $(DATA_DIR)

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
