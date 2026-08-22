# Makefile for libpelicanclient, the C interface to the Pelican client.
#
# Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
# SPDX-License-Identifier: Apache-2.0

# ABI_VERSION is the shared library's soname/compatibility version.  Bump
# it only for an incompatible change to include/pelican/client.h; additive
# changes (new functions) keep the same soname.
ABI_VERSION := 0

# VERSION labels the pkg-config metadata; it has no bearing on the ABI.
# The leading "v" of a release tag is stripped, since pkg-config compares
# versions numerically.
VERSION := $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev))

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  SOEXT      := dylib
  LIBNAME    := libpelicanclient.$(ABI_VERSION).$(SOEXT)
  # The library is built with an @rpath install name so it can be used
  # straight from the build tree, and `make install` rewrites that to the
  # absolute libdir path.  Reserve header space up front, or that rewrite
  # fails for any path longer than the original.
  GO_LDFLAGS := -ldflags '-extldflags "-Wl,-headerpad_max_install_names"'
else
  SOEXT      := so
  LIBNAME    := libpelicanclient.$(SOEXT).$(ABI_VERSION)
  # Record the soname so dependents load the versioned file, letting an
  # incompatible future release install alongside this one.
  GO_LDFLAGS := -ldflags '-extldflags "-Wl,-soname,$(LIBNAME)"'
endif
LINKNAME := libpelicanclient.$(SOEXT)

BUILDDIR := build
LIB      := $(BUILDDIR)/$(LIBNAME)
LINK     := $(BUILDDIR)/$(LINKNAME)
PCFILE   := $(BUILDDIR)/libpelicanclient.pc
EXAMPLE  := $(BUILDDIR)/pelican_example
ASYNC_EXAMPLE := $(BUILDDIR)/pelican_async_example
INTEGRATION_CLIENT := $(BUILDDIR)/integration_client

# Installation layout; override PREFIX/LIBDIR or use DESTDIR for staging.
PREFIX     ?= /usr/local
LIBDIR     ?= $(PREFIX)/lib
INCLUDEDIR ?= $(PREFIX)/include
PKGCONFIGDIR ?= $(LIBDIR)/pkgconfig
INSTALL    ?= install

GO_SOURCES := $(wildcard *.go) bridge.c bridge.h include/pelican/client.h go.mod go.sum

# Link examples and tests against the build tree; the loader finds the
# soname there thanks to the $(LINKNAME) symlink beside it.
CLIENT_LDFLAGS := -L$(BUILDDIR) -lpelicanclient -Wl,-rpath,$(abspath $(BUILDDIR))

.PHONY: all example integration-client integration-test test install uninstall clean print-abi-version FORCE

# Set explicitly: the phony helpers below would otherwise make whichever
# rule happens to appear first the default goal.
.DEFAULT_GOAL := all

all: $(LIB) $(LINK) $(PCFILE)

FORCE:

# Consumed by scripts/package-release.sh so the ABI version lives in one
# place.
print-abi-version:
	@echo $(ABI_VERSION)

$(LIB): $(GO_SOURCES)
	mkdir -p $(BUILDDIR)
	go build -buildmode=c-shared $(GO_LDFLAGS) -o $@ .
# The cgo-generated header exposes only internal pelicanc_* symbols; the
# public header is include/pelican/client.h.  cgo names it after the
# output file with the final extension replaced.
	rm -f $(BUILDDIR)/$(basename $(LIBNAME)).h
ifeq ($(UNAME_S),Darwin)
	install_name_tool -id @rpath/$(LIBNAME) $@
endif

$(LINK): $(LIB)
	ln -sf $(LIBNAME) $@

# Regenerated unconditionally: its contents depend on PREFIX/LIBDIR/
# VERSION, which are command-line variables rather than files, so a
# stale copy would otherwise survive `make install PREFIX=...`.
$(PCFILE): libpelicanclient.pc.in FORCE
	mkdir -p $(BUILDDIR)
	sed -e 's|@PREFIX@|$(PREFIX)|g' \
	    -e 's|@LIBDIR@|$(LIBDIR)|g' \
	    -e 's|@INCLUDEDIR@|$(INCLUDEDIR)|g' \
	    -e 's|@VERSION@|$(VERSION)|g' \
	    $< > $@

example: $(EXAMPLE) $(ASYNC_EXAMPLE)

$(EXAMPLE): $(LINK) examples/pelican_example.c
	$(CC) -Wall -Wextra -o $@ examples/pelican_example.c -Iinclude $(CLIENT_LDFLAGS)

$(ASYNC_EXAMPLE): $(LINK) examples/pelican_async_example.c
	$(CC) -Wall -Wextra -o $@ examples/pelican_async_example.c -Iinclude $(CLIENT_LDFLAGS)

integration-client: $(INTEGRATION_CLIENT)

$(INTEGRATION_CLIENT): $(LINK) tests/integration_client.c
	$(CC) -Wall -Wextra -o $@ tests/integration_client.c -Iinclude \
	    $(CLIENT_LDFLAGS) -lpthread

# Offline smoke test: version string, init, and an error-path check.
test: $(EXAMPLE) $(ASYNC_EXAMPLE)
	./$(EXAMPLE)

# Federation integration test: launches an in-process Pelican federation
# on the pure-Go serving paths (no XRootD needed) and drives the C
# library against it.
integration-test: $(INTEGRATION_CLIENT)
	go test -tags=integration -count=1 -timeout=20m -v ./integration

install: all
	$(INSTALL) -d $(DESTDIR)$(LIBDIR) $(DESTDIR)$(INCLUDEDIR)/pelican $(DESTDIR)$(PKGCONFIGDIR)
	$(INSTALL) -m 755 $(LIB) $(DESTDIR)$(LIBDIR)/$(LIBNAME)
	ln -sf $(LIBNAME) $(DESTDIR)$(LIBDIR)/$(LINKNAME)
	$(INSTALL) -m 644 include/pelican/client.h $(DESTDIR)$(INCLUDEDIR)/pelican/client.h
	$(INSTALL) -m 644 $(PCFILE) $(DESTDIR)$(PKGCONFIGDIR)/libpelicanclient.pc
ifeq ($(UNAME_S),Darwin)
# An installed dylib is found by absolute path, not by the consumer's rpath.
	install_name_tool -id $(LIBDIR)/$(LIBNAME) $(DESTDIR)$(LIBDIR)/$(LIBNAME)
endif

uninstall:
	rm -f $(DESTDIR)$(LIBDIR)/$(LIBNAME) $(DESTDIR)$(LIBDIR)/$(LINKNAME)
	rm -f $(DESTDIR)$(INCLUDEDIR)/pelican/client.h
	rm -f $(DESTDIR)$(PKGCONFIGDIR)/libpelicanclient.pc

clean:
	rm -rf $(BUILDDIR)
