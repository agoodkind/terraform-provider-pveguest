VERSION := 0.1.0
BINARY := terraform-provider-pveguest
PROVIDER_HOST := tofu.home.arpa
PROVIDER_NAMESPACE := agoodkind
PROVIDER_ADDRESS := $(PROVIDER_HOST)/$(PROVIDER_NAMESPACE)/pveguest

GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
MIRROR_DIRECTORY := $(HOME)/.terraform.d/plugins/$(PROVIDER_ADDRESS)/$(VERSION)/$(GOOS)_$(GOARCH)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test testacc check install

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

test:
	go test ./...

# The acceptance tests change a real guest. PVEGUEST_ACC_NODE,
# PVEGUEST_ACC_HOST, PVEGUEST_ACC_VMID, and PVEGUEST_ACC_KIND select it.
# The tests share one guest and its apt lock, which requires -p 1.
testacc: install
	TF_ACC=1 \
	TF_ACC_TERRAFORM_PATH="$$(command -v tofu)" \
	TF_ACC_PROVIDER_HOST=$(PROVIDER_HOST) \
	TF_ACC_PROVIDER_NAMESPACE=$(PROVIDER_NAMESPACE) \
	go test ./internal/acceptance/... -count=1 -p 1 -v -timeout 30m

check:
	go vet ./...
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt reports unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

install:
	mkdir -p "$(MIRROR_DIRECTORY)"
	go build -ldflags "$(LDFLAGS)" -o "$(MIRROR_DIRECTORY)/$(BINARY)_v$(VERSION)" .
