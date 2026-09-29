// Package projects owns account-scoped project operations.
package projects

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"shortlog-server/internal/db"
)

var (
	ErrInvalid  = errors.New("invalid project")
	ErrNotFound = errors.New("project not found")
	ErrArchived = errors.New("project is archived")
)

type Store struct{ queries *db.Queries }

func New(pool *pgxpool.Pool) *Store { return &Store{queries: db.New(pool)} }

type CreateInput struct {
	Name        string
	Description *string
}

type PatchInput struct {
	Name           *string
	Description    *string
	DescriptionSet bool
}

func validName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	return name, name != "" && utf8.RuneCountInString(name) <= 120 && !strings.ContainsFunc(name, unicode.IsControl)
}

func validDescription(value *string) (pgtype.Text, bool) {
	if value == nil {
		return pgtype.Text{}, true
	}
	text := strings.TrimSpace(*value)
	if utf8.RuneCountInString(text) > 4000 || strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t'
	}) {
		return pgtype.Text{}, false
	}
	if text == "" {
		return pgtype.Text{}, true
	}
	return pgtype.Text{String: text, Valid: true}, true
}

func (s *Store) Create(ctx context.Context, accountID pgtype.UUID, input CreateInput) (db.Project, error) {
	name, ok := validName(input.Name)
	if !ok {
		return db.Project{}, ErrInvalid
	}
	description, ok := validDescription(input.Description)
	if !ok {
		return db.Project{}, ErrInvalid
	}
	return s.queries.CreateProject(ctx, db.CreateProjectParams{
		AccountID: accountID, Name: name, Description: description,
	})
}

func (s *Store) List(ctx context.Context, accountID pgtype.UUID, archived bool) ([]db.Project, error) {
	if archived {
		return s.queries.ListArchivedProjects(ctx, accountID)
	}
	return s.queries.ListActiveProjects(ctx, accountID)
}

func (s *Store) Get(ctx context.Context, accountID, projectID pgtype.UUID) (db.Project, error) {
	project, err := s.queries.GetProject(ctx, db.GetProjectParams{ID: projectID, AccountID: accountID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Project{}, ErrNotFound
	}
	return project, err
}

func (s *Store) Patch(ctx context.Context, accountID, projectID pgtype.UUID, input PatchInput) (db.Project, error) {
	if input.Name == nil && !input.DescriptionSet {
		return db.Project{}, ErrInvalid
	}
	var name string
	if input.Name != nil {
		var ok bool
		name, ok = validName(*input.Name)
		if !ok {
			return db.Project{}, ErrInvalid
		}
	}
	var description pgtype.Text
	if input.DescriptionSet {
		var ok bool
		description, ok = validDescription(input.Description)
		if !ok {
			return db.Project{}, ErrInvalid
		}
	}
	project, err := s.queries.UpdateProject(ctx, db.UpdateProjectParams{
		ID: projectID, AccountID: accountID, NameSet: input.Name != nil, Name: name,
		DescriptionSet: input.DescriptionSet, Description: description,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		current, lookupErr := s.Get(ctx, accountID, projectID)
		if lookupErr != nil {
			return db.Project{}, lookupErr
		}
		if current.ArchivedAt.Valid {
			return db.Project{}, ErrArchived
		}
		return db.Project{}, ErrNotFound
	}
	return project, err
}

func (s *Store) Archive(ctx context.Context, accountID, projectID pgtype.UUID) error {
	count, err := s.queries.ArchiveProject(ctx, db.ArchiveProjectParams{ID: projectID, AccountID: accountID})
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err = s.Get(ctx, accountID, projectID)
	return err // Already archived is an idempotent success.
}

func (s *Store) Unarchive(ctx context.Context, accountID, projectID pgtype.UUID) error {
	count, err := s.queries.UnarchiveProject(ctx, db.UnarchiveProjectParams{ID: projectID, AccountID: accountID})
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err = s.Get(ctx, accountID, projectID)
	return err // Already active is an idempotent success.
}
