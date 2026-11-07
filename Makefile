SECRETS := hack/secrets-env
SB      := go run ./cmd/sb
RUN     ?=

.PHONY: help test build e2e-hetzner e2e-node-loss reaper reaper-schedule reaper-unschedule reaper-unlock reaper-status tenant-up tenant-down tenant-list tenant-ssh check prototype-up control-push

help:
	@echo "make test           unit tests and vet (L0)"
	@echo "make e2e-hetzner    real tenant on Hetzner: create, verify whoami over TLS, destroy (L2)"
	@echo "make e2e-node-loss  3-node install with Vaultwarden and Umami: power off a server, measure recovery (L2, ~35 min)"
	@echo "make reaper         delete expired development resources"
	@echo "make reaper-schedule    run the reaper every 15 minutes (macOS launchd); reaper-unschedule removes it"
	@echo "make reaper-unlock      unlock the dev keychain for the scheduled reaper (after each reboot)"
	@echo "make reaper-status      scheduled reaper state and log"
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
	$(SECRETS) $(SB) dev reap -wait 20m
	$(SECRETS) go test -tags e2e -count=1 -timeout 60m -v -run TestSmoke ./test/e2e/hetzner/

e2e-node-loss: build
	$(SECRETS) $(SB) dev reap -wait 20m
	$(SECRETS) go test -tags e2e -count=1 -timeout 100m -v -run TestNodeLoss ./test/e2e/hetzner/

reaper:
	$(SECRETS) $(SB) dev reap -wait 20m

# Scheduled reaper: a launchd agent running a prebuilt sb every 15 minutes.
# See "Scheduled reaper" in docs/development.md.
reaper-schedule:
	hack/reaper-schedule install

reaper-unschedule:
	hack/reaper-schedule uninstall

reaper-unlock:
	hack/reaper-schedule unlock

reaper-status:
	hack/reaper-schedule status

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
