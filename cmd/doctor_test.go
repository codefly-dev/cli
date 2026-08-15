package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorFlagsEmptyManifestOwnedServiceCodeWithRepairCommand(t *testing.T) {
	root := t.TempDir()
	writeDoctorTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), `name: doctor-test
layout: modules
modules:
  - name: saas
`)
	writeDoctorTestFile(t, filepath.Join(root, "modules", "saas", "module.codefly.yaml"), `kind: module
name: saas
services:
  - name: accounts
`)
	code := "package main\n"
	digest := sha256.Sum256([]byte(code))
	writeDoctorTestFile(t, filepath.Join(root, "modules", "saas", "tools", "base-manifest.json"),
		`{"files":{"services/accounts/code/main.go":"`+hex.EncodeToString(digest[:])+`"}}`)
	if err := os.MkdirAll(filepath.Join(root, "modules", "saas", "services", "accounts", "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	result := checkModuleServiceCode(context.Background())
	if result.status != statusFail {
		t.Fatalf("status = %v, want FAIL: %#v", result.status, result)
	}
	if !strings.Contains(result.detail, "saas/accounts") || result.fix != "`codefly sync module saas --restore-code`" {
		t.Fatalf("unexpected doctor result: %#v", result)
	}

	writeDoctorTestFile(t, filepath.Join(root, "modules", "saas", "services", "accounts", "code", "main.go"), code)
	result = checkModuleServiceCode(context.Background())
	if result.status != statusOK {
		t.Fatalf("restored code status = %v, want OK: %#v", result.status, result)
	}
}

func writeDoctorTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
