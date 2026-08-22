# Makefile for libpelicanclient, the C interface to the Pelican client.
#
# Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
# SPDX-License-Identifier: Apache-2.0

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  SOEXT := dylib
else
  SOEXT := so
endif

BUILDDIR := build
LIB      := $(BUILDDIR)/libpelicanclient.$(SOEXT)
EXAMPLE  := $(BUILDDIR)/pelican_example
ASYNC_EXAMPLE := $(BUILDDIR)/pelican_async_example
INTEGRATION_CLIENT := $(BUILDDIR)/integration_client

GO_SOURCES := $(wildcard *.go) bridge.c bridge.h include/pelican/client.h go.mod go.sum

.PHONY: all example integration-client integration-test test clean

all: $(LIB)

$(LIB): $(GO_SOURCES)
	mkdir -p $(BUILDDIR)
	go build -buildmode=c-shared -o $@ .
# The cgo-generated header exposes only internal pelicanc_* symbols; the
# public header is include/pelican/client.h.
	rm -f $(BUILDDIR)/libpelicanclient.h
ifeq ($(UNAME_S),Darwin)
	install_name_tool -id @rpath/libpelicanclient.dylib $@
endif

example: $(EXAMPLE) $(ASYNC_EXAMPLE)

$(EXAMPLE): $(LIB) examples/pelican_example.c
	$(CC) -Wall -Wextra -o $@ examples/pelican_example.c -Iinclude \
	    -L$(BUILDDIR) -lpelicanclient -Wl,-rpath,$(abspath $(BUILDDIR))

$(ASYNC_EXAMPLE): $(LIB) examples/pelican_async_example.c
	$(CC) -Wall -Wextra -o $@ examples/pelican_async_example.c -Iinclude \
	    -L$(BUILDDIR) -lpelicanclient -Wl,-rpath,$(abspath $(BUILDDIR))

integration-client: $(INTEGRATION_CLIENT)

$(INTEGRATION_CLIENT): $(LIB) tests/integration_client.c
	$(CC) -Wall -Wextra -o $@ tests/integration_client.c -Iinclude \
	    -L$(BUILDDIR) -lpelicanclient -Wl,-rpath,$(abspath $(BUILDDIR)) \
	    -lpthread

# Offline smoke test: version string, init, and an error-path check.
test: $(EXAMPLE) $(ASYNC_EXAMPLE)
	./$(EXAMPLE)

# Federation integration test: launches an in-process Pelican federation
# (requires XRootD binaries on PATH) and drives the C library against it.
integration-test: $(INTEGRATION_CLIENT)
	go test -tags=integration -count=1 -timeout=20m -v ./integration

clean:
	rm -rf $(BUILDDIR)
