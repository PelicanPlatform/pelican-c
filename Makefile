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

GO_SOURCES := $(wildcard *.go) bridge.c bridge.h include/pelican/client.h go.mod go.sum

.PHONY: all example test clean

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

example: $(EXAMPLE)

$(EXAMPLE): $(LIB) examples/pelican_example.c
	$(CC) -Wall -Wextra -o $@ examples/pelican_example.c -Iinclude \
	    -L$(BUILDDIR) -lpelicanclient -Wl,-rpath,$(abspath $(BUILDDIR))

# Offline smoke test: version string, init, and an error-path check.
test: $(EXAMPLE)
	./$(EXAMPLE)

clean:
	rm -rf $(BUILDDIR)
