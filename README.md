# Shortlog server

Go API for Shortlog. PostgreSQL is authoritative; the TUI will use the API rather than connect to the database directly.

## Toolchain

- Go 1.27.1
- PostgreSQL 18.6 for local development, running with Apple's `container` CLI (not Docker Desktop)
- sqlc 1.31.1 and Goose 3.28.0

Install the pinned database tools if they are not already available:

```sh
go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
go install github.com/pressly/goose/v3/cmd/goose@v3.28.0
```

The module currently uses the local import path `shortlog-server`. Set its final repository import path before publishing it.

## Local development

Start Apple's container service and a PostgreSQL container. The named volume preserves development data when the container is stopped. The password below is **only** for local development.

```sh
container system start
container volume create shortlog-postgres-data
container run -d --name shortlog-postgres \
  -e POSTGRES_USER=shortlog \
  -e POSTGRES_PASSWORD=shortlog_dev \
  -e POSTGRES_DB=shortlog \
  -p 127.0.0.1:5432:5432 \
  -v shortlog-postgres-data:/var/lib/postgresql \
  docker.io/library/postgres:18.6-alpine
```

Copy `.env.example` to `.env` and load its values in your shell before running the commands below. The server does not read `.env` automatically. For example, in zsh:

```sh
set -a
source .env
set +a
goose -dir db/migrations postgres "$DATABASE_URL" up
sqlc generate
go run ./cmd/shortlog-server
```

Check `http://127.0.0.1:8080/healthz` for process health or `/readyz` for database readiness. Stop the development database with `container stop shortlog-postgres` and resume it with `container start shortlog-postgres`.

Run tests with `go test ./...`. Migrations run as an explicit step; the API does not run them on startup.

## Scope

This is the entry-point skeleton: connection setup, health endpoints, an initial accounts migration, and sqlc configuration. Authentication, Inbox, notes, and production deployment are not implemented yet. Production credentials, TLS, and off-server PostgreSQL backups must be configured before deployment.
