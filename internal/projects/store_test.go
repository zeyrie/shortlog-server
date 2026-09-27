package projects

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"shortlog-server/internal/db"
)

func TestProjectValidation(t *testing.T) {
	for _, input := range []CreateInput{
		{}, {Name: "   "}, {Name: strings.Repeat("x", 121)}, {Name: "one\ntwo"},
	} {
		if _, ok := validName(input.Name); ok {
			t.Fatalf("accepted project name %q", input.Name)
		}
	}
	if name, ok := validName("  Work / Go  "); !ok || name != "Work / Go" {
		t.Fatalf("normalized name = %q, %v", name, ok)
	}
	tooLong := strings.Repeat("x", 4001)
	if _, ok := validDescription(&tooLong); ok {
		t.Fatal("accepted oversized description")
	}
	blank := "  "
	if description, ok := validDescription(&blank); !ok || description.Valid {
		t.Fatal("blank description should be null")
	}
}

func TestProjectLifecyclePostgres(t *testing.T) {
	url := os.Getenv("SHORTLOG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHORTLOG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	query := db.New(pool)
	owner, err := query.CreateAccount(ctx, db.CreateAccountParams{Username: "Project owner", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, "DELETE FROM accounts WHERE id=$1", owner.ID); err != nil {
			t.Error(err)
		}
	})
	other, err := query.CreateAccount(ctx, db.CreateAccountParams{Username: "Other owner", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, "DELETE FROM accounts WHERE id=$1", other.ID); err != nil {
			t.Error(err)
		}
	})
	store := New(pool)
	description := "First description"
	first, err := store.Create(ctx, owner.ID, CreateInput{Name: "  Work  ", Description: &description})
	if err != nil || first.Name != "Work" || first.Description.String != description || first.ArchivedAt.Valid {
		t.Fatalf("create = %+v, %v", first, err)
	}
	second, err := store.Create(ctx, owner.ID, CreateInput{Name: "Work"})
	if err != nil || second.Description.Valid {
		t.Fatalf("duplicate name or null description = %+v, %v", second, err)
	}
	if _, err := store.Create(ctx, owner.ID, CreateInput{Name: "   "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid create = %v", err)
	}
	active, err := store.List(ctx, owner.ID, false)
	if err != nil || len(active) != 2 {
		t.Fatalf("active projects = %+v, %v", active, err)
	}
	if _, err := store.Get(ctx, other.ID, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner's get = %v", err)
	}
	if _, err := store.Patch(ctx, other.ID, first.ID, PatchInput{DescriptionSet: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner's patch = %v", err)
	}
	name := "Renamed"
	updated, err := store.Patch(ctx, owner.ID, first.ID, PatchInput{Name: &name})
	if err != nil || updated.Name != name || updated.Description.String != description || updated.UpdatedAt.Time.Before(first.UpdatedAt.Time) {
		t.Fatalf("name patch = %+v, %v", updated, err)
	}
	updated, err = store.Patch(ctx, owner.ID, first.ID, PatchInput{DescriptionSet: true})
	if err != nil || updated.Description.Valid || updated.Name != name {
		t.Fatalf("clear description = %+v, %v", updated, err)
	}
	if err := store.Archive(ctx, owner.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, owner.ID, first.ID); err != nil {
		t.Fatalf("repeat archive = %v", err)
	}
	archived, err := store.List(ctx, owner.ID, true)
	if err != nil || len(archived) != 1 || archived[0].ID != first.ID {
		t.Fatalf("archived list = %+v, %v", archived, err)
	}
	active, err = store.List(ctx, owner.ID, false)
	if err != nil || len(active) != 1 || active[0].ID != second.ID {
		t.Fatalf("active list after archive = %+v, %v", active, err)
	}
	if _, err := store.Get(ctx, owner.ID, first.ID); err != nil {
		t.Fatalf("archived project unreadable: %v", err)
	}
	if _, err := store.Patch(ctx, owner.ID, first.ID, PatchInput{Name: &name}); !errors.Is(err, ErrArchived) {
		t.Fatalf("archived patch = %v", err)
	}
	if err := store.Archive(ctx, other.ID, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner's archive = %v", err)
	}
	if err := store.Unarchive(ctx, owner.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Unarchive(ctx, owner.ID, first.ID); err != nil {
		t.Fatalf("repeat unarchive = %v", err)
	}
	if _, err := store.Patch(ctx, owner.ID, first.ID, PatchInput{Name: &name}); err != nil {
		t.Fatalf("patch after unarchive = %v", err)
	}
}
