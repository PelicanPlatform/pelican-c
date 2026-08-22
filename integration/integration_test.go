//go:build integration

/***************************************************************
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

// Package integration drives the compiled C library (via the
// tests/integration_client driver) against a real, in-process Pelican
// federation launched with the upstream fed_test_utils harness.  The
// federation uses the pure-Go serving paths (posixv2 origin, V2 cache),
// so no XRootD installation is required.
//
// Requires the driver already built:
//
//	make integration-client
//	go test -tags=integration -v ./integration
package integration

import (
	"bytes"
	"context"
	"crypto/md5"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/fed_test_utils"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/token"
	"github.com/pelicanplatform/pelican/token_scopes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helloContent = "Hello, World!" // written by fed_test_utils into every export

//go:embed resources/fed_config.yaml
var fedConfig string

// runDriver executes one integration_client subcommand and returns its
// stdout, stderr, and exit code.
func runDriver(t *testing.T, driver string, env []string, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, driver, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}
	}
	t.Logf("driver %v: exit=%d\nstdout:\n%s\nstderr:\n%s", args, exitCode,
		stdout.String(), stderr.String())
	return stdout.String(), stderr.String(), exitCode
}

// findValue returns the value of the first `key<value>` line in the
// driver's output, or "" if absent.
func findValue(t *testing.T, output, key string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), key); ok {
			return after
		}
	}
	return ""
}

func TestCClientFederation(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	require.NoError(t, err)
	driver := filepath.Join(repoRoot, "build", "integration_client")
	if _, err := os.Stat(driver); err != nil {
		t.Fatalf("driver %s not built; run `make integration-client` first", driver)
	}

	// The upstream launchers unconditionally run `xrootd -v` as a version
	// gate (launcher_utils.CheckDefaults → xrootd.CheckXrootdEnv), even
	// for the pure-Go posixv2/CacheV2 paths used here, which never execute
	// XRootD.  Until that check is gated on backends that actually launch
	// XRootD, satisfy it with a stub binary.
	stubDir := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(stubDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(stubDir, "xrootd"),
		[]byte("#!/bin/sh\necho \"v5.8.2\"\n"), 0755))
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fed := fed_test_utils.NewFedTest(t, fedConfig)
	require.NotEmpty(t, fed.Exports)
	namespace := fed.Exports[0].FederationPrefix

	fedUrlStr := param.Server_ExternalWebUrl.GetString()
	fedUrl, err := url.Parse(fedUrlStr)
	require.NoError(t, err)
	objectUrl := func(p string) string {
		return "pelican://" + fedUrl.Host + path.Join(namespace, p)
	}

	// Mint a token with read+write over the whole namespace, delivered
	// to the C client through WLCG bearer-token discovery.
	issuer, err := config.GetServerIssuerURL()
	require.NoError(t, err)
	tokConf := token.NewWLCGToken()
	tokConf.Lifetime = 15 * time.Minute
	tokConf.Issuer = issuer
	tokConf.Subject = "pelican-c-integration"
	tokConf.AddAudienceAny()
	tokConf.AddResourceScopes(
		token_scopes.NewResourceScope(token_scopes.Wlcg_Storage_Read, "/"),
		token_scopes.NewResourceScope(token_scopes.Wlcg_Storage_Modify, "/"),
	)
	tok, err := tokConf.CreateToken()
	require.NoError(t, err)
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(tok), 0600))

	// The driver must see only the test federation: strip any ambient
	// Pelican configuration and point HOME somewhere empty.  TLS is
	// fully verified — the client trusts the federation's generated CA
	// (Server.TLSCACertificateFile is added to the system pool); skipping
	// verification in tests is forbidden, as it has hidden real bugs.
	caFile := param.Server_TLSCACertificateFile.GetString()
	require.NotEmpty(t, caFile)
	require.FileExists(t, caFile)
	env := []string{
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"PELICAN_FEDERATION_DISCOVERYURL=" + fedUrlStr,
		"PELICAN_SERVER_TLSCACERTIFICATEFILE=" + caFile,
		"BEARER_TOKEN_FILE=" + tokenFile,
	}
	if v, ok := os.LookupEnv("DYLD_FALLBACK_LIBRARY_PATH"); ok {
		env = append(env, "DYLD_FALLBACK_LIBRARY_PATH="+v)
	}

	downloads := t.TempDir()

	t.Run("stat", func(t *testing.T) {
		stdout, _, code := runDriver(t, driver, env, "stat", objectUrl("hello_world.txt"))
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, fmt.Sprintf("size=%d\n", len(helloContent)))
		assert.Contains(t, stdout, "is_collection=0")
	})

	t.Run("sync-get", func(t *testing.T) {
		dest := filepath.Join(downloads, "sync.txt")
		stdout, _, code := runDriver(t, driver, env, "get", objectUrl("hello_world.txt"), dest)
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, fmt.Sprintf("bytes=%d", len(helloContent)))
		content, err := os.ReadFile(dest)
		require.NoError(t, err)
		assert.Equal(t, helloContent, string(content))
	})

	t.Run("async-get", func(t *testing.T) {
		dest := filepath.Join(downloads, "async.txt")
		stdout, _, code := runDriver(t, driver, env, "get-async", objectUrl("hello_world.txt"), dest)
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, fmt.Sprintf("bytes=%d", len(helloContent)))
		// The driver exits nonzero on any off-thread callback; assert on
		// the counters anyway so a report regression is loud.
		assert.Contains(t, stdout, "progress_off_thread=0")
		assert.NotContains(t, stdout, "progress_calls=0\n")
		content, err := os.ReadFile(dest)
		require.NoError(t, err)
		assert.Equal(t, helloContent, string(content))
	})

	t.Run("checksum-request", func(t *testing.T) {
		dest := filepath.Join(downloads, "checksummed.txt")
		stdout, _, code := runDriver(t, driver, env, "get", objectUrl("hello_world.txt"), dest,
			"checksum=md5", "require-checksum")
		require.Equal(t, 0, code)
		wantMd5 := md5.Sum([]byte(helloContent))
		assert.Contains(t, stdout, "checksum=md5:"+hex.EncodeToString(wantMd5[:]))
	})

	t.Run("checksum-unknown-digest", func(t *testing.T) {
		dest := filepath.Join(downloads, "never-written.txt")
		_, stderr, code := runDriver(t, driver, env, "get", objectUrl("hello_world.txt"), dest,
			"checksum=bogus-digest")
		require.NotEqual(t, 0, code)
		assert.Contains(t, stderr, "unknown checksum digest")
	})

	// Preferred-cache selection: by default only the listed caches are
	// tried; a trailing "+" falls back to the director's list.  The
	// endpoint to prefer is discovered from an ordinary transfer rather
	// than assumed, because Cache.Url in this federation carries a path
	// prefix (/api/v1.0/cache/data/...) that preferred-cache handling
	// does not preserve.
	t.Run("preferred-cache", func(t *testing.T) {
		object := objectUrl("hello_world.txt")

		stdout, _, code := runDriver(t, driver, env, "get", object,
			filepath.Join(downloads, "discover.txt"))
		require.Equal(t, 0, code)
		endpoint := findValue(t, stdout, "endpoint=")
		require.NotEmpty(t, endpoint, "transfer did not report an endpoint")

		t.Run("honored", func(t *testing.T) {
			dest := filepath.Join(downloads, "via-preferred.txt")
			stdout, _, code := runDriver(t, driver, env, "get", object, dest,
				"cache=https://"+endpoint)
			require.Equal(t, 0, code)
			assert.Contains(t, stdout, "endpoint="+endpoint)
			content, err := os.ReadFile(dest)
			require.NoError(t, err)
			assert.Equal(t, helloContent, string(content))
		})

		// 127.0.0.1:1 refuses connections, so a transfer that only has
		// this cache to work with must fail: no silent fallback to the
		// director's servers.
		t.Run("exclusive-without-plus", func(t *testing.T) {
			_, stderr, code := runDriver(t, driver, env, "get", object,
				filepath.Join(downloads, "never-written-2.txt"),
				"cache=https://127.0.0.1:1")
			require.NotEqual(t, 0, code)
			assert.Contains(t, stderr, "get failed")
			assert.NotContains(t, stderr, endpoint,
				"director-provided endpoint was tried despite no '+' fallback")
		})

		t.Run("plus-falls-back", func(t *testing.T) {
			dest := filepath.Join(downloads, "via-fallback.txt")
			stdout, _, code := runDriver(t, driver, env, "get", object, dest,
				"cache=https://127.0.0.1:1", "cache=+")
			require.Equal(t, 0, code)
			assert.Contains(t, stdout, "endpoint="+endpoint)
			content, err := os.ReadFile(dest)
			require.NoError(t, err)
			assert.Equal(t, helloContent, string(content))
		})
	})

	t.Run("fs-read", func(t *testing.T) {
		stdout, _, code := runDriver(t, driver, env, "fs-read", objectUrl("hello_world.txt"))
		require.Equal(t, 0, code)
		assert.Equal(t, helloContent, stdout)
	})

	// The single-shot asynchronous API, driven through poll() the way an
	// event-loop host would.
	t.Run("async-stat", func(t *testing.T) {
		stdout, _, code := runDriver(t, driver, env, "stat-async", objectUrl("hello_world.txt"))
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, fmt.Sprintf("size=%d\n", len(helloContent)))
		assert.Contains(t, stdout, "is_collection=0")
	})

	t.Run("async-list", func(t *testing.T) {
		stdout, _, code := runDriver(t, driver, env, "list-async", objectUrl("/"))
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, "hello_world.txt")
	})

	// Opens, reads to EOF, and closes using only non-blocking calls; the
	// driver also asserts that a second operation on a busy handle is
	// refused rather than corrupting the stream.
	t.Run("async-fs-read", func(t *testing.T) {
		stdout, stderr, code := runDriver(t, driver, env, "fs-read-async", objectUrl("hello_world.txt"))
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, helloContent)
		assert.Contains(t, stdout, "busy_rejected=1")
		assert.Contains(t, stderr, fmt.Sprintf("fs_read_bytes=%d", len(helloContent)))
	})

	t.Run("list", func(t *testing.T) {
		stdout, _, code := runDriver(t, driver, env, "list", objectUrl("/"))
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, "hello_world.txt")
	})

	uploadContent := strings.Repeat("pelican-c integration upload\n", 64)

	t.Run("put-then-read-back", func(t *testing.T) {
		src := filepath.Join(downloads, "upload-src.txt")
		require.NoError(t, os.WriteFile(src, []byte(uploadContent), 0644))
		remote := objectUrl("pelican-c-upload.txt")

		stdout, _, code := runDriver(t, driver, env, "put", src, remote)
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, fmt.Sprintf("bytes=%d", len(uploadContent)))

		stdout, _, code = runDriver(t, driver, env, "fs-read", remote)
		require.Equal(t, 0, code)
		assert.Equal(t, uploadContent, stdout)
	})

	t.Run("third-party-copy", func(t *testing.T) {
		src := objectUrl("hello_world.txt")
		dest := objectUrl("pelican-c-copied.txt")

		stdout, _, code := runDriver(t, driver, env, "copy", src, dest)
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, "object=")

		stdout, _, code = runDriver(t, driver, env, "fs-read", dest)
		require.Equal(t, 0, code)
		assert.Equal(t, helloContent, stdout)
	})

	t.Run("delete", func(t *testing.T) {
		remote := objectUrl("pelican-c-upload.txt")

		_, _, code := runDriver(t, driver, env, "delete", remote)
		require.Equal(t, 0, code)

		// The object must now be gone from the origin.  Stat with
		// ?directread: the earlier read-back cached the object, and a
		// plain stat is happily answered by the cache's lingering copy.
		_, stderr, code := runDriver(t, driver, env, "stat", remote+"?directread")
		require.NotEqual(t, 0, code, "stat of deleted object should fail")
		assert.Contains(t, stderr, "stat failed")
	})
}
