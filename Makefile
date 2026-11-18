TOFU ?= $(shell command -v tofu)
SCHEMA_DIR := .docs-schema

.PHONY: build test testacc docs

build:
	go build -o terraform-provider-golambda .

test:
	go test ./...

# Acceptance and network tests: real toolchain downloads and builds, run with OpenTofu.
testacc:
	TF_ACC=1 TF_ACC_TERRAFORM_PATH=$(TOFU) TF_ACC_PROVIDER_HOST=registry.opentofu.org \
	GOLAMBDA_NETWORK_TESTS=1 go test -timeout 30m ./...

# Generates docs/ from the schema and examples. tfplugindocs normally runs
# terraform itself, so hand it the schema from tofu instead.
docs: build
	mkdir -p $(SCHEMA_DIR)
	printf 'provider_installation {\n  dev_overrides {\n    "spacelift.io/moyer/golambda" = "%s"\n  }\n  direct {}\n}\n' "$(CURDIR)" > $(SCHEMA_DIR)/tofurc
	printf 'terraform {\n  required_providers {\n    golambda = { source = "spacelift.io/moyer/golambda" }\n  }\n}\n' > $(SCHEMA_DIR)/main.tf
	# tfplugindocs only looks for the provider under its registry.terraform.io address.
	cd $(SCHEMA_DIR) && TF_CLI_CONFIG_FILE=tofurc $(TOFU) providers schema -json \
		| sed 's#"spacelift.io/moyer/golambda"#"registry.terraform.io/hashicorp/golambda"#' > schema.json
	go tool tfplugindocs generate \
		--provider-name golambda --providers-schema $(SCHEMA_DIR)/schema.json
