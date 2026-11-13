# make install   build the UI (bin/build) and install gwag to $(BINDIR)
# make install-bin   install without rebuilding the UI
# make uninstall     remove the installed binary
#
# bin/build stays the single build entry point; this file only calls it.

PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
# DESTDIR stages the tree for packagers: $(DESTDIR)$(BINDIR)/gwag.
DESTDIR ?=
INSTDIR  = $(DESTDIR)$(BINDIR)

# Same stamp goreleaser applies (.goreleaser.yaml). git describe gives
# the nearest tag; a checkout with no tags builds as devel.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS  = -s -w -X github.com/iodesystems/gwag/v2/gw.buildVersion=$(VERSION)

.PHONY: install install-bin uninstall bindir

# Sequenced in the recipe, not as prerequisites, so `make -j` cannot
# embed the UI mid-build.
install:
	bin/build --no-go
	@$(MAKE) --no-print-directory install-bin

install-bin:
	@test -f ui/dist/index.html || bin/build --no-ui
	@mkdir -p $(INSTDIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(INSTDIR)/gwag.tmp ./gw/cmd/gwag
	mv -f $(INSTDIR)/gwag.tmp $(INSTDIR)/gwag
	@echo "installed gwag $(VERSION) to $(INSTDIR)"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "note: $(BINDIR) is not on PATH";; esac
	@echo "running as a service: gwag service restart"

uninstall:
	rm -f $(INSTDIR)/gwag

bindir:
	@echo $(BINDIR)
