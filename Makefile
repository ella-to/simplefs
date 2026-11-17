# simplefs build, test, and demo targets.
#
# Portable (any OS):
#   make test       run the Go tests
#   make race       run the Go tests with the race detector
#   make vet        go vet ./...
#   make fmt        fail if any file is not gofmt-clean
#   make cross      vet every package for every supported GOOS/GOARCH
#   make bench      run the benchmarks
#   make check      fmt + vet + race + cross (the full verification gate)
#   make example    run the in-memory example program (no mount)
#
# macOS File Provider demo (Darwin only):
#   make            build + sign the app bundle
#   make install    deploy to ~/Applications and register the extension
#   make mount      add the File Provider domain and expose it at MOUNT_PATH
#                   (default ~/simplefs). Override:  make mount MOUNT_PATH=~/mydir
#   make unmount    remove the domain (and the MOUNT_PATH symlink)
#   make settings   open the pane to enable the domain (macOS 15.2+)
#   make events     tail create/update/delete events as Go receives them
#   make clean      remove build artifacts
#
# Linux / Docker (the NFS backend — no FUSE, no privileges to serve):
#   make docker-image   build the containerized NFS server image
#   make docker-serve   run it, published on port 2049
#   make docker-test    end-to-end: server container + privileged client
#                       container that kernel-mounts it and exercises file I/O
#
# Code signing identity. Ad-hoc ("-") is the default. Override like:
#   make IDENTITY="Apple Development: you@example.com (TEAMID)"

.DEFAULT_GOAL := all

.PHONY: test race vet fmt cross bench check example

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

cross:
	sh scripts/cross-vet.sh

bench:
	go test -run '^$$' -bench . -benchmem ./...

check: fmt vet race cross

example:
	go run ./examples/memfs

ifeq ($(shell uname -s),Darwin)

SDK      := $(shell xcrun --show-sdk-path)
ARCH     := $(shell uname -m)
IDENTITY ?= -
MINVER   := 11.0

CAPI     := fileprovider/capi
EXT_DIR  := fileprovider/host/ext
APP_DIR  := fileprovider/host/app
ARCHIVE_PKG := ./examples/fileprovider/archive
APP_PKG  := ./examples/fileprovider/app

# Where the mounted folder is exposed (a symlink to the real location macOS
# picks under ~/Library/CloudStorage). Override: make mount MOUNT_PATH=~/mydir
MOUNT_PATH ?= $(HOME)/simplefs

BUILD    := build
APP      := $(BUILD)/SimpleFS.app
APPEXE   := $(APP)/Contents/MacOS/SimpleFS
APPEX    := $(APP)/Contents/PlugIns/FileProviderExt.appex
EXTEXE   := $(APPEX)/Contents/MacOS/FileProviderExt
ARCHIVE  := $(BUILD)/libprovider.a

# File Provider extensions must live in /Applications or ~/Applications to show
# up in System Settings. ~/Applications needs no root.
DEST     := $(HOME)/Applications
DEST_APP := $(DEST)/SimpleFS.app
DEST_EXE := $(DEST_APP)/Contents/MacOS/SimpleFS
DEST_EX  := $(DEST_APP)/Contents/PlugIns/FileProviderExt.appex

EXT_ID   := to.ella.simplefs.ext

CFLAGS   := -fobjc-arc -mmacosx-version-min=$(MINVER) -isysroot $(SDK) -arch $(ARCH) -Wall
EXT_FW   := -framework Foundation -framework FileProvider -framework UniformTypeIdentifiers \
            -framework CoreFoundation -framework Security

EXT_SRC  := $(EXT_DIR)/SimpleFSItem.m $(EXT_DIR)/SimpleFSEnumerator.m $(EXT_DIR)/FileProviderExtension.m
GO_SRC   := $(shell find . -name '*.go' ! -name '*_test.go' ! -path './$(BUILD)/*')
LSREGISTER := /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister

.PHONY: all archive bundle sign install mount unmount settings events clean

all: sign

# 1. Build the registered provider + capi bridge as a C archive.
$(ARCHIVE): $(GO_SRC)
	@mkdir -p $(BUILD)
	CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=$(MINVER) \
		CGO_CFLAGS="-mmacosx-version-min=$(MINVER)" CGO_LDFLAGS="-mmacosx-version-min=$(MINVER)" \
		go build -buildmode=c-archive -o $(ARCHIVE) $(ARCHIVE_PKG)

archive: $(ARCHIVE)

# 2. Assemble the .app/.appex bundle: the container executable is a Go binary
#    (examples/fileprovider/app, which drives the fileprovider package), and the extension is
#    the Obj-C host linked with the Go archive against the hand-written
#    bridge.h.
bundle: $(ARCHIVE)
	@rm -rf $(APP)
	@mkdir -p $(APP)/Contents/MacOS $(APPEX)/Contents/MacOS
	cp $(APP_DIR)/Info.plist $(APP)/Contents/Info.plist
	cp $(EXT_DIR)/Info.plist $(APPEX)/Contents/Info.plist
	CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=$(MINVER) \
		CGO_CFLAGS="-mmacosx-version-min=$(MINVER)" CGO_LDFLAGS="-mmacosx-version-min=$(MINVER)" \
		go build -o $(APPEXE) $(APP_PKG)
	clang $(CFLAGS) -e _NSExtensionMain -I$(EXT_DIR) -I$(CAPI) \
		$(EXT_SRC) $(ARCHIVE) $(EXT_FW) -o $(EXTEXE)

# 3. Sign the extension first, then the app (inside-out).
sign: bundle
	codesign --force --timestamp=none --sign "$(IDENTITY)" \
		--entitlements $(EXT_DIR)/ext.entitlements "$(APPEX)"
	codesign --force --timestamp=none --sign "$(IDENTITY)" \
		--entitlements $(APP_DIR)/app.entitlements "$(APP)"
	@echo "Signed with identity: $(IDENTITY)"

# 4. Deploy to ~/Applications and let the system discover the extension.
install: sign
	@mkdir -p "$(DEST)"
	-$(LSREGISTER) -u "$(DEST_APP)" 2>/dev/null
	rm -rf "$(DEST_APP)"
	cp -R "$(APP)" "$(DEST_APP)"
	$(LSREGISTER) -f "$(DEST_APP)"
	# Launch once so Launch Services fully registers the bundle; without this
	# pluginkit registration of an ad-hoc-signed appex does not stick.
	open "$(DEST_APP)"
	sleep 3
	pluginkit -a "$(DEST_EX)"
	pluginkit -e use -i $(EXT_ID)
	@echo "Installed to $(DEST_APP). Our extension:"
	@pluginkit -m -i $(EXT_ID) | sed 's/^/  /' || echo "  NOT registered"

# 5. Mount: add the domain and expose it at MOUNT_PATH (a symlink to the real
#    location macOS chooses under ~/Library/CloudStorage).
#
#    macOS 15.2+: new third-party domains start DISABLED. After this you must
#    enable it once:  System Settings > General > Login Items & Extensions >
#    Extensions > File Providers (i) > toggle SimpleFS on.  `make settings` opens
#    that pane.
mount: install
	"$(DEST_EXE)" --path "$(MOUNT_PATH)"

unmount:
	"$(DEST_EXE)" --remove --path "$(MOUNT_PATH)" 2>/dev/null || \
		"$(APPEXE)" --remove --path "$(MOUNT_PATH)"

settings:
	open "x-apple.systempreferences:com.apple.LoginItems-Settings.extension"

# Tail the create/update/delete events as the Go provider reports them
# (written by examples/fileprovider/archive via the WatchProvider capability).
EVENTLOG := $(HOME)/Library/Containers/$(EXT_ID)/Data/events.log
events:
	@touch "$(EVENTLOG)" 2>/dev/null || true
	tail -f "$(EVENTLOG)"

clean:
	rm -rf $(BUILD)

else

all: test

clean:
	rm -rf build

endif

# ---- Linux / Docker (NFS backend) ------------------------------------------

DOCKER_IMAGE ?= simplefs-nfs

.PHONY: docker-image docker-serve docker-test

docker-image:
	docker build -t $(DOCKER_IMAGE) .

docker-serve: docker-image
	docker run --rm -p 2049:2049 $(DOCKER_IMAGE)

docker-test: docker-image
	sh scripts/docker-e2e.sh
