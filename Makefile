SECRETS := hack/secrets-env
SB      := go run ./cmd/sb
RUN     ?=

.PHONY: help test build e2e-hetzner reaper tenant-up tenant-down tenant-list tenant-ssh check

help:
	@echo "make test           unit tests and vet (L0)"
	@echo "make e2e-hetzner    real tenant on Hetzner: create, verify whoami over TLS, destroy (L2)"
	@echo "make reaper         delete expired development resources"
	@echo "make tenant-up      create a tenant and leave it running (expires after SB_TTL, default 2h)"
	@echo "make tenant-down RUN=<run-id>"
	@echo "make tenant-list    list labelled resources by run"
	@echo "make tenant-ssh RUN=<run-id>   print an ssh command for node 1"
	@echo "make check          test + secret scan of tracked files"

test:
	go vet ./...
	go test ./...

build:
	go build -o bin/sb ./cmd/sb

# Runs the reaper first so leftovers from crashed runs never pile up.
e2e-hetzner:
	$(SECRETS) $(SB) dev reap
	$(SECRETS) go test -tags e2e -count=1 -timeout 60m -v ./test/e2e/hetzner/

reaper:
	$(SECRETS) $(SB) dev reap

tenant-up:
	$(SECRETS) $(SB) dev tenant up

tenant-down:
	@test -n "$(RUN)" || (echo "usage: make tenant-down RUN=<run-id>" && exit 2)
	$(SECRETS) $(SB) dev tenant down $(RUN)

tenant-list:
	$(SECRETS) $(SB) dev tenant list

tenant-ssh:
	@test -n "$(RUN)" || (echo "usage: make tenant-ssh RUN=<run-id>" && exit 2)
	$(SECRETS) $(SB) dev tenant ssh $(RUN)

check: test
	hack/scan-secrets
