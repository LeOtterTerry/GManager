package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GMANAGER_TOKEN", "short-token")
	if _, err := ResolveToken(dir); err == nil {
		t.Fatal("short GMANAGER_TOKEN was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "token")); !os.IsNotExist(err) {
		t.Fatal("short token created a file")
	}

	const envToken = "dev-placeholder-token-0123456789"
	if len(envToken) < MinTokenLength {
		t.Fatalf("fixture length = %d", len(envToken))
	}
	t.Setenv("GMANAGER_TOKEN", envToken)
	got, err := ResolveToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != envToken {
		t.Fatalf("token = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "token")); !os.IsNotExist(err) {
		t.Fatal("env token wrote a file")
	}

	t.Setenv("GMANAGER_TOKEN", "")
	generated, err := ResolveToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) < MinTokenLength || strings.Contains(generated, " ") {
		t.Fatalf("generated token length = %d", len(generated))
	}
	info, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	again, err := ResolveToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again != generated {
		t.Fatal("second read rotated the token")
	}

	shortDir := filepath.Join(dir, "short")
	if err := os.MkdirAll(shortDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shortDir, "token"), []byte("too-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveToken(filepath.Join(dir, "short")); err == nil {
		t.Fatal("short token file was accepted")
	}
}

func TestDataDirFor(t *testing.T) {
	if got := dataDirFor("linux", ""); got != "data" {
		t.Fatalf("linux = %q", got)
	}
	if got := dataDirFor("windows", `D:\ProgramData`); got != `D:\ProgramData\GManager` {
		t.Fatalf("windows = %q", got)
	}
	if got := dataDirFor("windows", ""); got != `C:\ProgramData\GManager` {
		t.Fatalf("windows fallback = %q", got)
	}
}
