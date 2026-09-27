// Package notes owns account-scoped note storage and Inbox/project placement.
package notes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"shortlog-server/internal/db"
)

var (
	ErrInvalid  = errors.New("invalid note")
	ErrNotFound = errors.New("note or project not found")
	ErrArchived = errors.New("project is archived")
)

type Store struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool, queries: db.New(pool)} }

type PatchInput struct {
	Content    *string
	ProjectID  pgtype.UUID
	ProjectSet bool
}

type Page struct {
	Items      []db.Note
	NextCursor string
}

type cursorValue struct {
	Version   int    `json:"v"`
	Account   string `json:"account"`
	Project   string `json:"project"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func validContent(content string) bool {
	return strings.TrimSpace(content) != "" && utf8.RuneCountInString(content) <= 20000 &&
		!strings.ContainsFunc(content, func(r rune) bool {
			return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
		})
}

func (s *Store) lockProject(ctx context.Context, queries *db.Queries, accountID, projectID pgtype.UUID) error {
	project, err := queries.LockNoteProject(ctx, db.LockNoteProjectParams{ID: projectID, AccountID: accountID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if project.ArchivedAt.Valid {
		return ErrArchived
	}
	return nil
}

func (s *Store) Create(ctx context.Context, accountID pgtype.UUID, content string, projectID pgtype.UUID) (db.Note, error) {
	if !validContent(content) {
		return db.Note{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.Note{}, err
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if projectID.Valid {
		if err := s.lockProject(ctx, queries, accountID, projectID); err != nil {
			return db.Note{}, err
		}
	}
	note, err := queries.CreateNote(ctx, db.CreateNoteParams{AccountID: accountID, Content: content, ProjectID: projectID})
	if err != nil {
		return db.Note{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Note{}, err
	}
	return note, nil
}

func (s *Store) Get(ctx context.Context, accountID, noteID pgtype.UUID) (db.Note, error) {
	note, err := s.queries.GetNote(ctx, db.GetNoteParams{ID: noteID, AccountID: accountID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Note{}, ErrNotFound
	}
	return note, err
}

func (s *Store) lockNote(ctx context.Context, queries *db.Queries, accountID, noteID pgtype.UUID) (db.Note, error) {
	note, err := queries.LockNote(ctx, db.LockNoteParams{ID: noteID, AccountID: accountID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Note{}, ErrNotFound
	}
	return note, err
}

func (s *Store) lockWritableProjects(ctx context.Context, queries *db.Queries, accountID, source, target pgtype.UUID) error {
	if !source.Valid && !target.Valid {
		return nil
	}
	if source.Valid && target.Valid && source != target {
		// Always lock both rows in UUID order to avoid opposite moves deadlocking.
		if bytes.Compare(source.Bytes[:], target.Bytes[:]) > 0 {
			source, target = target, source
		}
		if err := s.lockProject(ctx, queries, accountID, source); err != nil {
			return err
		}
		return s.lockProject(ctx, queries, accountID, target)
	}
	if source.Valid {
		return s.lockProject(ctx, queries, accountID, source)
	}
	return s.lockProject(ctx, queries, accountID, target)
}

func (s *Store) Patch(ctx context.Context, accountID, noteID pgtype.UUID, input PatchInput) (db.Note, error) {
	if input.Content == nil && !input.ProjectSet {
		return db.Note{}, ErrInvalid
	}
	if input.Content != nil && !validContent(*input.Content) {
		return db.Note{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.Note{}, err
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	current, err := s.lockNote(ctx, queries, accountID, noteID)
	if err != nil {
		return db.Note{}, err
	}
	target := current.ProjectID
	if input.ProjectSet {
		target = input.ProjectID
	}
	if err := s.lockWritableProjects(ctx, queries, accountID, current.ProjectID, target); err != nil {
		return db.Note{}, err
	}
	content := current.Content
	if input.Content != nil {
		content = *input.Content
	}
	note, err := queries.UpdateNote(ctx, db.UpdateNoteParams{ID: noteID, AccountID: accountID, Content: content, ProjectID: target})
	if err != nil {
		return db.Note{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Note{}, err
	}
	return note, nil
}

func (s *Store) Delete(ctx context.Context, accountID, noteID pgtype.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	current, err := s.lockNote(ctx, queries, accountID, noteID)
	if err != nil {
		return err
	}
	if current.ProjectID.Valid {
		if err := s.lockProject(ctx, queries, accountID, current.ProjectID); err != nil {
			return err
		}
	}
	count, err := queries.DeleteNote(ctx, db.DeleteNoteParams{ID: noteID, AccountID: accountID})
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func decodeCursor(encoded string, accountID, projectID pgtype.UUID) (pgtype.Timestamptz, pgtype.UUID, error) {
	if encoded == "" {
		return pgtype.Timestamptz{}, pgtype.UUID{}, nil
	}
	if len(encoded) > 1024 {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value cursorValue
	if err := decoder.Decode(&value); err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrInvalid
	}
	project := ""
	if projectID.Valid {
		project = projectID.String()
	}
	if value.Version != 1 || value.Account != accountID.String() || value.Project != project {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrInvalid
	}
	created, err := time.Parse(time.RFC3339Nano, value.CreatedAt)
	if err != nil || created.UTC().Format(time.RFC3339Nano) != value.CreatedAt {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrInvalid
	}
	var id pgtype.UUID
	if err := id.Scan(value.ID); err != nil || !id.Valid || id.String() != value.ID {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrInvalid
	}
	return pgtype.Timestamptz{Time: created, Valid: true}, id, nil
}

func encodeCursor(note db.Note, accountID, projectID pgtype.UUID) (string, error) {
	project := ""
	if projectID.Valid {
		project = projectID.String()
	}
	raw, err := json.Marshal(cursorValue{Version: 1, Account: accountID.String(), Project: project,
		CreatedAt: note.CreatedAt.Time.UTC().Format(time.RFC3339Nano), ID: note.ID.String()})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Store) List(ctx context.Context, accountID, projectID pgtype.UUID, limit int, cursor string) (Page, error) {
	if limit < 1 || limit > 100 {
		return Page{}, ErrInvalid
	}
	if projectID.Valid {
		_, err := s.queries.GetProject(ctx, db.GetProjectParams{ID: projectID, AccountID: accountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return Page{}, ErrNotFound
		}
		if err != nil {
			return Page{}, err
		}
	}
	createdAt, cursorID, err := decodeCursor(cursor, accountID, projectID)
	if err != nil {
		return Page{}, err
	}
	var rows []db.Note
	if projectID.Valid {
		rows, err = s.queries.ListProjectNotes(ctx, db.ListProjectNotesParams{
			AccountID: accountID, ProjectID: projectID, CursorAt: createdAt, CursorID: cursorID, PageSize: int32(limit + 1),
		})
	} else {
		rows, err = s.queries.ListInboxNotes(ctx, db.ListInboxNotesParams{
			AccountID: accountID, CursorAt: createdAt, CursorID: cursorID, PageSize: int32(limit + 1),
		})
	}
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		page.NextCursor, err = encodeCursor(rows[limit-1], accountID, projectID)
		if err != nil {
			return Page{}, err
		}
	}
	return page, nil
}
