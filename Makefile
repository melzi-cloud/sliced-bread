SECRETS := hack/secrets-env
SB      := go run ./cmd/sb
RUN     ?=

.PHONY: help test build e2e-hetzner reaper tenant-up tenant-down tenant-list tenant-ssh check prototype-up control-push

help:
	@echo "make test           unit tests and vet (L0)"
	@echo "make e2e-hetzner    real tenant on Hetzner: create, verify whoami over TLS, destroy (L2)"
	@echo "make reaper         delete expired development resources"
	@echo "make tenant-up      create a tenant and leave it running (expires after SB_TTL, default 2h)"
	@echo "make tenant-down RUN=<run-id>"
	@echo "make tenant-list    list labelled resources by run"
	@echo "make tenant-ssh RUN=<run-id>   print an ssh command for node 1"
	@echo "make check          test + secret scan of tracked files"
	@echo "make prototype-up   tenant with the full SlicedBread platform and trusted certificates"
	@echo "make control-push RUN=<run-id>   rebuild sb-control and roll it out"

test:
	go vet ./...
	go test ./...

# Release-style build: sb with the linux/amd64 sb-control embedded.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o internal/controlbin/payload/sb-control ./cmd/sb-control
	go build -trimpath -o bin/sb ./cmd/sb

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

prototype-up: build
	$(SECRETS) $(SB) dev prototype up

control-push:
	@test -n "$(RUN)" || (echo "usage: make control-push RUN=<run-id>" && exit 2)
	$(SECRETS) $(SB) dev control push $(RUN)
