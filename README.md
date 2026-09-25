# Shortlog server

Go API for Shortlog. PostgreSQL is authoritative; the TUI will use the API rather than connect to the database directly.

## Toolchain

- Go 1.27.1
- PostgreSQL 18.6 for local development, running with Apple's `container` CLI (not Docker Desktop)
- sqlc 1.31.1, Goose 3.28.0, and Task 3.53.1

Install the pinned database tools if they are not already available:

```sh
go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
go install github.com/pressly/goose/v3/cmd/goose@v3.28.0
go install github.com/go-task/task/v3/cmd/task@v3.53.1
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

Copy `.env.example` to `.env` for local development. `.env` is ignored by Git. The server loads it from the project root when `DATABASE_URL` is not set, including when launched through GoLand. Task uses the same file:

```sh
task db:migrate
task generate
task dev
```

To debug directly in GoLand, use a **Go Build** run/debug configuration with:

- **Run kind:** Package; **Package path:** `shortlog-server/cmd/shortlog-server`
- **Working directory:** the `shortlog-server` project root

Use GoLand's **Debug** action; no program arguments are needed. A supplied `DATABASE_URL` takes priority over `.env`. Run `task --list` to see the available commands. Check `http://127.0.0.1:8080/healthz` for process health or `/readyz` for database readiness. Stop the development database with `container stop shortlog-postgres` and resume it with `container start shortlog-postgres`.

Run tests with `task test`. Migrations run as an explicit step; the API does not run them on startup.

To start over during early development, run `task db:reset`. After confirmation it drops and recreates **only** the local `shortlog` database in the `shortlog-postgres` Apple container, then reapplies all migrations. It disconnects active database clients and deletes local application data, but keeps the container and named volume. The task refuses to run if `DATABASE_URL` is not the known local development URL. Do not run it against a database whose contents you need to keep.

## Production environment

Set `APP_ENV=production`, `DATABASE_URL`, and `HTTP_ADDR=:8080` in Dokploy's environment/secret settings. Production does not use an env file. `APP_ENV=production` disables local `.env` loading; `.dockerignore` also excludes env files from container build contexts. Keep PostgreSQL credentials out of Git and do not reuse the local development password. TLS termination and production database access still need to be configured.

## Scope

This is the entry-point skeleton: connection setup, health endpoints, account/authentication tables, and sqlc configuration. Authentication flows, Inbox, notes, and production deployment are not implemented yet. The Telegram PKCE verifier column expects application-encrypted data; do not store a plaintext verifier. Production credentials, TLS, and off-server PostgreSQL backups must be configured before deployment.
