package bootstrap_test

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/bootstrap"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// TestBootstrap_FromURL exercises the remote-fetch path against a
// local httptest server. We don't need a real agent binary — the
// install just has to land bytes at the right path; the
// "is the agent runnable" half is the bootstrap_test.go upload path
// and is independent.
func TestBootstrap_FromURL(t *testing.T) {
	if testing.Short() {
		t.Skip("bootstrap: skipping FromURL test under -short")
	}
	t.Parallel()

	want := []byte("#!/bin/sh\necho remote-fetched-agent\n")
	hash := sha256.Sum256(want)
	hashHex := hex.EncodeToString(hash[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(want)
	}))
	defer srv.Close()

	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	installPath := filepath.Join(t.TempDir(), "ptyrelay-agent")
	got, err := bootstrap.Bootstrap(ctx, sb, bootstrap.Options{
		FromURL: func(_, _ string) (string, string) {
			return srv.URL + "/ptyrelay-agent", hashHex
		},
		InstallPath: installPath,
	})
	if err != nil {
		t.Fatalf("Bootstrap FromURL: %v", err)
	}
	if got != installPath {
		t.Errorf("got %q want %q", got, installPath)
	}
	body, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(want) {
		t.Errorf("fetched bytes mismatch:\ngot:  %q\nwant: %q", body, want)
	}
	st, _ := os.Stat(installPath)
	if st.Mode().Perm()&0o100 == 0 {
		t.Errorf("agent not executable, mode = %o", st.Mode().Perm())
	}
}

func TestBootstrap_FromURL_GzipUnpacks(t *testing.T) {
	if testing.Short() {
		t.Skip("bootstrap: skipping FromURL test under -short")
	}
	t.Parallel()

	payload := []byte("#!/bin/sh\necho gz-payload\n")
	// Serve the gzip-compressed form; bootstrap should gunzip it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(payload)
		_ = gz.Close()
	}))
	defer srv.Close()

	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	installPath := filepath.Join(t.TempDir(), "ptyrelay-agent")
	_, err := bootstrap.Bootstrap(ctx, sb, bootstrap.Options{
		FromURL: func(_, _ string) (string, string) {
			// Trailing .gz triggers the gunzip block.
			return srv.URL + "/ptyrelay-agent.gz", ""
		},
		InstallPath: installPath,
	})
	if err != nil {
		t.Fatalf("Bootstrap FromURL gz: %v", err)
	}
	body, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(payload) {
		t.Errorf("gunzipped mismatch:\ngot:  %q\nwant: %q", body, payload)
	}
}

func TestBootstrap_FromURL_SHA256Mismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("bootstrap: skipping FromURL test under -short")
	}
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("real payload"))
	}))
	defer srv.Close()

	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	installPath := filepath.Join(t.TempDir(), "ptyrelay-agent")
	_, err := bootstrap.Bootstrap(ctx, sb, bootstrap.Options{
		FromURL: func(_, _ string) (string, string) {
			// Hash for "different payload" — guaranteed mismatch.
			return srv.URL + "/agent", strings.Repeat("0", 64)
		},
		InstallPath: installPath,
	})
	if err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256") && !strings.Contains(err.Error(), "fetch failed") {
		t.Errorf("expected sha256 mismatch in error, got: %v", err)
	}
	if _, statErr := os.Stat(installPath); !os.IsNotExist(statErr) {
		t.Errorf("partial install left at %s on mismatch", installPath)
	}
}
