default: fmt lint install generate

build:
	go build -v ./...

install: build
	go install -v ./...

# dev-install builds + installs the provider for LOCAL testing and prints the
# dev_overrides block (with your Go bin path filled in) to drop into
# ~/.terraformrc, so `terraform plan`/`apply` use this local build with no
# registry and no `terraform init`. See CONTRIBUTING.md.
dev-install: install
	@bin="$$(go env GOBIN)"; [ -n "$$bin" ] || bin="$$(go env GOPATH)/bin"; \
	printf '\nInstalled terraform-provider-barndoor to %s\n' "$$bin"; \
	printf 'Add this to ~/.terraformrc (or a file pointed to by TF_CLI_CONFIG_FILE):\n\n'; \
	printf 'provider_installation {\n'; \
	printf '  dev_overrides {\n'; \
	printf '    "registry.terraform.io/barndoor-ai/barndoor" = "%s"\n' "$$bin"; \
	printf '  }\n'; \
	printf '  direct {}\n'; \
	printf '}\n\n'

lint:
	golangci-lint run

generate:
	cd tools; go generate ./...

fmt:
	gofmt -s -w -e .

test:
	go test -v -cover -timeout=120s -parallel=10 ./...

testacc:
	TF_ACC=1 go test -v -cover -timeout 120m ./...

.PHONY: fmt lint test testacc build install dev-install generate
