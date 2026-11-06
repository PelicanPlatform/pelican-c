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

package main

import (
	"net/url"
	"testing"
)

// The names that matter are the ones whose encoded and decoded
// spellings differ.  A plain name round-trips through either form, so
// it can never catch this; '%' fails loudly and '#' and '?' fail
// silently, which is why they are each here by name.
var remotesNeedingEscapes = []string{
	"pelican://example.org/ns/plain.txt",
	"pelican://example.org:8443/ns/plain.txt",
	"pelican://example.org/ns/with%20space.txt",
	"pelican://example.org/ns/literal%25.txt",
	"pelican://example.org/ns/hash%23frag.txt",
	"pelican://example.org/ns/question%3Fmark.txt",
	"pelican://example.org/ns/%E6%97%A5%E6%9C%AC%E8%AA%9E.txt",
	"pelican://example.org/ns/back%5Cslash.txt",
	"pelican://example.org/ns/dir%20with%20space/child.txt",
	"osdf://example.org/ns/a%25b%23c.txt",
}

// TestSplitRemoteForFSRoundTrips is the property the split exists to
// have: PelicanFS.OpenFile joins the prefix and the name back together
// and parses the result, so the join has to reproduce the URL exactly.
func TestSplitRemoteForFSRoundTrips(t *testing.T) {
	for _, remote := range remotesNeedingEscapes {
		u, err := url.Parse(remote)
		if err != nil {
			t.Fatalf("%q does not parse: %v", remote, err)
		}
		prefix, name := splitRemoteForFS(u)
		if got := prefix + name; got != remote {
			t.Errorf("split of %q rejoins as %q", remote, got)
		}
	}
}

// TestSplitRemoteForFSSurvivesReparsing guards the failure actually
// seen: the rejoined URL is parsed a second time inside OpenFile, and
// it has to name the same object it started as.  Checking only that
// the second parse succeeds would miss '#' and '?', which succeed and
// silently name something else.
func TestSplitRemoteForFSSurvivesReparsing(t *testing.T) {
	for _, remote := range remotesNeedingEscapes {
		u, err := url.Parse(remote)
		if err != nil {
			t.Fatalf("%q does not parse: %v", remote, err)
		}
		prefix, name := splitRemoteForFS(u)
		again, err := url.Parse(prefix + name)
		if err != nil {
			t.Errorf("rejoined %q does not parse: %v", remote, err)
			continue
		}
		if again.Path != u.Path {
			t.Errorf("%q names %q after a round trip, was %q",
				remote, again.Path, u.Path)
		}
		if again.Fragment != "" || again.RawQuery != "" {
			t.Errorf("%q grew a fragment %q / query %q on the round trip",
				remote, again.Fragment, again.RawQuery)
		}
	}
}

// TestSplitRemoteForFSRejectsDecodedPath is the regression itself: the
// bug was passing u.Path, and this records why that cannot come back.
func TestSplitRemoteForFSRejectsDecodedPath(t *testing.T) {
	u, err := url.Parse("pelican://example.org/ns/literal%25.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, name := splitRemoteForFS(u); name == u.Path {
		t.Fatalf("split returned the decoded path %q; OpenFile cannot reparse it", name)
	}
}
