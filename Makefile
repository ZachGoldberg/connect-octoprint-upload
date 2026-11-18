# Build, test and package octoprint-upload.
#
#   make core      fetch the pinned Prusa Connect checkout into .core/
#   make check     what CI runs: go vet and the tests
#   make release   the release directory, in release/ (what the marketplace's dry run checks)
#   make dist      release/ plus checksums.txt, packed as dist/<name>-<version>.tar.gz for `module add -from`
#   make install   install release/ into a throwaway Prusa Connect data dir in bin/try, to try it
#
# Until the Prusa Connect SDK is published, the module builds against a
# checkout of Prusa Connect at CORE_REF (go.mod replaces the SDK with .core/sdk).
# Raise CORE_REF to build against a newer core, and raise `require.core` in
# module.yaml with it. .core may also be a symlink to a checkout you already have.

CORE_REPO ?= https://github.com/prusa3d/prusa-connect.git
CORE_REF ?= v0.1.0-beta.1
CORE := .core

NAME := $(shell sed -n 's/^name:[[:space:]]*//p' module.yaml | head -1)
VERSION := $(shell sed -n 's/^version:[[:space:]]*//p' module.yaml | head -1)
MODULE := $(shell sed -n 's/^module[[:space:]]*//p' go.mod | head -1)
PLATFORMS := $(shell sh scripts/platforms.sh)

RELEASE := release
DIST := dist
ARCHIVE := $(DIST)/$(NAME)-$(VERSION).tar.gz

.PHONY: core check release dist install clean

# The checkout is cloned once. To move to another CORE_REF, `rm -rf .core`
# first.
$(CORE)/sdk/go.mod:
	git -c advice.detachedHead=false clone --quiet --depth 1 --branch $(CORE_REF) $(CORE_REPO) $(CORE)

core: $(CORE)/sdk/go.mod

check: core
	go vet ./...
	go test ./...

# Each binary is built the way the marketplace's builder builds it: CGO off,
# -trimpath, stripped, so it runs on any host of its platform and names no
# path of the machine that built it.
release: core
	rm -rf $(RELEASE)
	mkdir -p $(RELEASE)
	cp module.yaml LICENSE README.md CHANGELOG.md $(RELEASE)/
	@for platform in $(PLATFORMS); do \
		goos=$${platform%%_*}; goarch=$${platform#*_}; \
		binary=$(NAME)_$$platform; [ $$goos = windows ] && binary=$$binary.exe; \
		echo "building $$binary"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -trimpath -ldflags='-s -w' -o $(RELEASE)/$$binary . || exit 1; \
		chmod +x $(RELEASE)/$$binary; \
	done
	@echo "$(NAME) $(VERSION) is in $(RELEASE)/"

# checksums.txt lists every file, and the archive holds the files themselves
# with no ./ entry, which `module add -from <archive>` takes.
dist: release
	cd $(RELEASE) && sha256sum $$(ls | grep -v '^checksums.txt$$') > checksums.txt
	mkdir -p $(DIST)
	tar -C $(RELEASE) -czf $(ARCHIVE) $$(cd $(RELEASE) && ls)
	@echo "$(ARCHIVE)"

# Installs release/ into bin/try with the pinned core's own `module add`, then
# prints how to start that core with it.
TRY := $(CURDIR)/bin/try
install: release
	mkdir -p $(TRY)
	printf 'core:\n  overlay: %s/resources.json\n  modules: %s/modules\n  authz:\n    database: %s/authz.db\n  api:\n    addr: 127.0.0.1:18480\n' \
		"$(TRY)" "$(TRY)" "$(TRY)" > $(TRY)/config.yaml
	go -C $(CORE) run ./cmd/prusa-connect module add "$(MODULE)@$(VERSION)" \
		-config "$(TRY)/config.yaml" -from "$(CURDIR)/$(RELEASE)" -approve -approve-listen
	@echo
	@echo "Installed. Build the core with its web app once:  make -C $(CORE) build"
	@echo "Then start it:                                    (cd $(TRY) && $(CURDIR)/$(CORE)/bin/server -config config.yaml)"
	@echo "The upload API then listens on port 17481."

clean:
	rm -rf $(RELEASE) $(DIST) bin
