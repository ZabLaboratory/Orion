package lsdp

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurity_NoLocalAuth is the Bastion gate for issue #25 / ADR 007
// §C.3b. The gateway-first non-negotiable
// (_shared/architecture.md §"NO local auth on microservices") forbids
// Orion from validating any credential. This package mounts the LSDP
// wire; it MUST derive identity ONLY through the configured AuthSource
// (HeaderAuthSource on the antenne = ZabGate's injected headers;
// localOperatorAuth on embedded-local = the loopback handshake), never a
// locally-validated credential, and MUST NEVER:
//
//   - import a JWT library;
//   - instantiate the kit's token Authenticator (server.StaticTokens,
//     server.AuthenticatorFunc, server.Authenticator);
//   - set Config.Auth on the kit server;
//   - read the Subscribe frame's Token field.
//
// This is a static source scan (fails on violation), not a prose audit.
func TestSecurity_NoLocalAuth(t *testing.T) {
	files := nonTestGoFiles(t)
	if len(files) == 0 {
		t.Fatal("no source files scanned — test is vacuous")
	}

	// Substrings that, if present anywhere in this package's non-test
	// source, signal a gateway-first violation.
	forbidden := []string{
		"jwt",            // any JWT verifier import/usage
		"Authenticator",  // kit token auth interface / func adapter
		"StaticTokens",   // kit dev token authenticator
		"Authenticate(",  // calling the token validate path
		"Auth:",          // setting Config.Auth on the kit server
		".Token",         // reading the Subscribe.Token field
		"subFrame.Token", // explicit token read
	}

	sawHeaderTrust := false
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		text := stripComments(t, f, src)
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Errorf("%s: forbidden token %q present — gateway-first violation (no local auth)", filepath.Base(f), bad)
			}
		}
		// Header-trust is proven by the default identity source being
		// HeaderAuthSource (which delegates to auth.FromHeaders, ADR 016
		// §3.2). The wire derives identity through the AuthSource seam, so
		// the literal auth.FromHeaders no longer appears in code — the
		// HeaderAuthSource default is the byte-for-byte equivalent.
		if strings.Contains(text, "auth.HeaderAuthSource") {
			sawHeaderTrust = true
		}
	}

	if !sawHeaderTrust {
		t.Error("identity does not default to auth.HeaderAuthSource anywhere in the LSDP wire — header-trust not proven")
	}
}

// nonTestGoFiles returns the package's non-test .go files.
func nonTestGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	return out
}

// stripComments returns the file's source code with comments removed,
// so a doc comment explaining WHY the token is ignored (which must name
// the forbidden tokens) doesn't trip the scan — only real code counts.
// It tokenises with go/scanner (comments off) and re-emits the literal
// token text verbatim, preserving `auth.FromHeaders` as a contiguous
// string so the positive header-trust check still matches.
func stripComments(t *testing.T, path string, src []byte) string {
	t.Helper()
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0) // mode 0 ⇒ comments are skipped
	var b strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if lit != "" {
			b.WriteString(lit)
		} else {
			b.WriteString(tok.String())
		}
	}
	return b.String()
}
