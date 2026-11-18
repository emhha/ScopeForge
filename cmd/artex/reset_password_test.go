package main

import (
	"github.com/Autumn-27/artex/db"
	"golang.org/x/crypto/bcrypt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteAdministratorPasswordReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artex.sqlite")
	t.Setenv("ARTEX_DB_PATH", path)
	t.Setenv("ARTEX_CONFIG", filepath.Join(dir, "missing.json"))
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if code := resetAdministratorPassword(dir, strings.NewReader("fixture-password\n")); code != 0 {
		t.Fatalf("exit code=%d", code)
	}
	store, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash, exists, err := store.GetSetting("auth.password_hash")
	if err != nil || !exists {
		t.Fatalf("password hash missing: %v", err)
	}
	if err = bcrypt.CompareHashAndPassword([]byte(hash), []byte("fixture-password")); err != nil {
		t.Fatal("login cannot verify reset password", err)
	}
}
