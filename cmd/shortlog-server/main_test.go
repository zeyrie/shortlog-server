package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLocalEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("DATABASE_URL=postgres://local\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("loads when no database environment is set", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "")
		t.Setenv("APP_ENV", "")
		if err := loadLocalEnv(path); err != nil {
			t.Fatal(err)
		}
		if got := os.Getenv("DATABASE_URL"); got != "postgres://local" {
			t.Fatalf("DATABASE_URL = %q, want local value", got)
		}
	})

	t.Run("does not override inherited credentials", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://inherited")
		if err := loadLocalEnv(path); err != nil {
			t.Fatal(err)
		}
		if got := os.Getenv("DATABASE_URL"); got != "postgres://inherited" {
			t.Fatalf("DATABASE_URL = %q, want inherited value", got)
		}
	})

	t.Run("production ignores the local file", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "")
		t.Setenv("APP_ENV", "production")
		if err := loadLocalEnv(path); err != nil {
			t.Fatal(err)
		}
		if got := os.Getenv("DATABASE_URL"); got != "" {
			t.Fatalf("DATABASE_URL = %q, want empty value", got)
		}
	})
}
