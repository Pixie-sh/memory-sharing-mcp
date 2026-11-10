# make build    build ./memory-sharing-mcp with the version from git (override: make build VERSION=v0.2.0)
# make setup    guided first-time setup: provider, store, config, service, then gen-config
# make install  build, then copy to ~/.local/bin and (re)start the service
#               (OS=Darwin, the default: LaunchAgent; OS=Linux: systemd user unit)
# make gen-config  give an agent its own key and config (AGENT=claude|opencode|pi|codex|<name>; asks if unset)
# make logs     follow the service log
# make readiness  service state, /readyz, and the installed version

VERSION ?= $(shell git describe --always --dirty)
BIN := memory-sharing-mcp
URL ?= http://127.0.0.1:8790
LABEL := sh.pixie.$(BIN)
OS ?= Darwin
ifneq ($(filter Linux linux,$(OS)),)
OSNAME := Linux
LOG := $(HOME)/.local/state/$(BIN)/$(BIN).log
STATUS := systemctl --user show $(LABEL).service -p ActiveState -p MainPID -p ExecMainStatus 2>/dev/null
else ifneq ($(filter Darwin darwin,$(OS)),)
OSNAME := Darwin
LOG := $(HOME)/Library/Logs/$(BIN).log
STATUS := launchctl print gui/$$(id -u)/$(LABEL) 2>/dev/null | grep -E '^\s*(state|pid|last exit code) =' | head -3
else
$(error OS must be Darwin or Linux, not $(OS))
endif

.PHONY: build setup install gen-config logs readiness

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/$(BIN)

setup: build
	@[ "$$(uname -s)" = $(OSNAME) ] || { echo "OS=$(OS), but this machine is $$(uname -s)"; exit 1; }
	@URL=$(URL) scripts/setup.sh ./$(BIN)

install: build
	@[ "$$(uname -s)" = $(OSNAME) ] || { echo "OS=$(OS), but this machine is $$(uname -s)"; exit 1; }
	./$(BIN) install

gen-config:
	@URL=$(URL) scripts/gen-config.sh

logs:
	tail -f $(LOG)

readiness:
	@$(STATUS) || echo "service: not loaded"
	@printf 'readyz: '; curl -s --max-time 2 $(URL)/readyz || printf 'no response at $(URL)'; echo
	@printf 'installed: '; $(HOME)/.local/bin/$(BIN) version 2>/dev/null || echo "not installed"
