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

Run `task test:db` for database-backed authentication and HTTP tests. It uses only the known local PostgreSQL URL; tests create their own accounts and remove them afterward.

To start over during early development, run `task db:reset`. After confirmation it drops and recreates **only** the local `shortlog` database in the `shortlog-postgres` Apple container, then reapplies all migrations. It disconnects active database clients and deletes local application data, but keeps the container and named volume. The task refuses to run if `DATABASE_URL` is not the known local development URL. Do not run it against a database whose contents you need to keep.

## Production environment

Set `APP_ENV=production`, `DATABASE_URL`, and `HTTP_ADDR=:8080` in Dokploy's environment/secret settings. Production does not use an env file. `APP_ENV=production` disables local `.env` loading; `.dockerignore` also excludes env files from container build contexts. Keep PostgreSQL credentials out of Git and do not reuse the local development password. TLS termination and production database access still need to be configured.

## Scope

The API has `/healthz`, `/readyz`, and four bearer-protected routes: `GET /v1/me`, `GET /v1/sessions`, `DELETE /v1/sessions/{id}`, and `POST /v1/sessions/revoke-all`. Sessions use opaque tokens, stored only as hashes in PostgreSQL. Activity is persisted at most once per 24 hours; a session expires 31 days after that persisted activity, so it stays valid at least 30 days and at most about 31 days after its actual last use. The device list's last-used time can lag by up to a day. Account/identity resolution and session issuance are internal services; **there is no public sign-in route yet**. Email OTP, Telegram OIDC, Inbox, notes, and production deployment are not implemented. The Telegram PKCE verifier column expects application-encrypted data; do not store a plaintext verifier. Production credentials, TLS, and off-server PostgreSQL backups must be configured before deployment.

API errors use a stable JSON envelope, for example `{"error":{"code":"unauthorized","message":"Authentication required."}}`. Clients should branch on `code` and HTTP status, not message text. Shared codes and safe messages live in `internal/apierror`; internal database or server details stay in logs. A 401 includes `WWW-Authenticate`, and a 405 includes `Allow`.

The current protected routes have a 10-second request deadline; the server also limits header reading to 5 seconds, request reading to 10 seconds, responses to 30 seconds, and idle connections to 60 seconds. Future long-polling endpoints must explicitly choose compatible deadlines. The verified-login trust-boundary requirements are recorded in `internal/auth/doc.go` before OTP or OIDC routes are added.
