# `make help` lists every target. Lint, build, test, and release come from the
# central go-makefile pipeline that bootstrap.mk fetches at parse time. Do NOT
# add project-local lint, deadcode, audit, fmt, vet, or staticcheck targets.

BINARY := terraform-provider-pveguest
CMD    := .

GO_MK_MODULES := go-build.mk go-release.mk

include bootstrap.mk

.DEFAULT_GOAL := check

PROVIDER_VERSION   := 0.1.0
PROVIDER_HOST      := tofu.home.arpa
PROVIDER_NAMESPACE := agoodkind
PROVIDER_ADDRESS   := $(PROVIDER_HOST)/$(PROVIDER_NAMESPACE)/pveguest
MIRROR_DIRECTORY   := $(HOME)/.terraform.d/plugins/$(PROVIDER_ADDRESS)/$(PROVIDER_VERSION)/$(shell go env GOOS)_$(shell go env GOARCH)

# Extra go test arguments for testacc, for example TESTARGS="-run TestAccLink".
TESTARGS ?=

.PHONY: install-mirror testacc

# OpenTofu reads this directory as an implied local mirror.
install-mirror:
	mkdir -p "$(MIRROR_DIRECTORY)"
	go build -ldflags "-X main.version=$(PROVIDER_VERSION)" \
		-o "$(MIRROR_DIRECTORY)/$(BINARY)_v$(PROVIDER_VERSION)" .

# The acceptance tests change a real guest. PVEGUEST_ACC_NODE,
# PVEGUEST_ACC_HOST, PVEGUEST_ACC_VMID, and PVEGUEST_ACC_KIND select it.
# The tests share one guest and its apt lock, which requires -p 1.
testacc: install-mirror
	TF_ACC=1 \
	TF_ACC_TERRAFORM_PATH="$$(command -v tofu)" \
	TF_ACC_PROVIDER_HOST=$(PROVIDER_HOST) \
	TF_ACC_PROVIDER_NAMESPACE=$(PROVIDER_NAMESPACE) \
	go test ./internal/acceptance/... -count=1 -p 1 -v -timeout 30m $(TESTARGS)
