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

The API has `/healthz`, `/readyz`, email OTP and Telegram OIDC login, and bearer-protected `GET /v1/me`, `PATCH /v1/me`, `POST /v1/auth/logout`, `GET /v1/sessions`, `DELETE /v1/sessions/{id}`, and `POST /v1/sessions/revoke-all`. Logout revokes only the current session; revoke-all signs out all devices. Sessions use opaque tokens, stored only as hashes in PostgreSQL. Tokens do not automatically rotate; activity is persisted at most once per 24 hours, and a session expires 31 days after that persisted activity, so it stays valid at least 30 days and at most about 31 days after its actual last use. The device list's last-used time can lag by up to a day. Account linking, Inbox, notes, and production deployment are not implemented. Telegram's PKCE verifier is encrypted using AES-GCM with a separate encryption key; never store plaintext. Production credentials, TLS, and off-server PostgreSQL backups must be configured before deployment.

Email OTP: set `EMAIL_OTP_KEY` to a stable 32-byte hex secret and `SMTP_HOST`, `SMTP_PORT=465`, `SMTP_USER`, `SMTP_FROM`, and `SMTP_PASSWORD` in local `.env` or deployment secrets. `.env.example` includes Hostinger settings for `shortlog@zeyrie.top`; add its **app password** to the ignored `.env` yourself. Without `SMTP_PASSWORD`, the server runs but email start returns 503; no OTP is logged or silently delivered. Never change the key while outstanding codes or restoration tickets must remain valid. Run `task db:migrate` for the rate-limit table before using login.

`POST /v1/auth/email/start` accepts `{"email":"you@example.com"}` and returns 202 with `challenge_id`; `POST /v1/auth/email/verify` accepts `{"challenge_id":"...","code":"12345678","username":"Ari","time_zone":"Asia/Kolkata"}`. Username (non-unique, 1–80 characters) and an IANA time zone are required **only when the verified email creates a new account**. Both columns are required in the database as well. If omitted for a new account, the API returns `422 profile_required` without consuming the code; retry verify with the profile before expiry. Existing accounts sign in with just challenge ID and code; profile values supplied on later sign-ins do not overwrite stored settings. To update an existing account, send `PATCH /v1/me` with `{"username":"Ari","time_zone":"Asia/Kolkata"}` and a bearer token. A successful verification returns `{"status":"signed_in","token":"..."}`. If the account is pending deletion, verification instead returns `{"status":"restore_required","recovery_ticket":"..."}`; **only after the user explicitly agrees**, send that ticket to `POST /v1/auth/email/restore` as `{"recovery_ticket":"..."}` to restore the account and receive a token. Codes expire in 10 minutes, allow five attempts, and are single-use. Restoration tickets are single-use and expire after 10 minutes; restoration is only possible during the account's 30-day recovery period. Start requests are limited per email and IP; clients should not log codes, tickets, or tokens.

The IP limiter uses the direct connection address, not untrusted `X-Forwarded-For`. Before putting the API behind a shared production reverse proxy, configure trusted-proxy client IP handling or enforce an additional per-client rate limit at the proxy; otherwise all clients behind that proxy share one IP quota. Restoring an account revokes its previous sessions and issues only the new session.

### Telegram OIDC login

Create a bot and use **BotFather → Login Widget** to register the exact HTTPS callback URL `https://YOUR_HOST/v1/auth/telegram/callback`. Obtain the **Client ID and Client Secret** there (not the Bot API token). Set `TELEGRAM_CLIENT_ID`, `TELEGRAM_CLIENT_SECRET`, `TELEGRAM_REDIRECT_URI` (the exact registered callback), and `TELEGRAM_ENCRYPTION_KEY` (a separate 32-byte key encoded as 64 hex characters). If unset, Telegram routes return 503 while email login remains available. Discovery is pinned to `https://oauth.telegram.org`; the server validates RS256-signed ID tokens against Telegram JWKS, issuer, audience, expiry, and the attempt's nonce. BotFather must use its default RS256 signing algorithm. See [Telegram's current OIDC documentation](https://core.telegram.org/bots/telegram-login).

The TUI calls `POST /v1/auth/telegram/start` (no body) and receives `attempt_id`, `poll_secret`, and `authorization_url`. It opens the URL in the user's browser; Telegram returns to the configured GET callback, which displays only a completion message. The TUI sends `POST /v1/auth/telegram/poll` with `{"attempt_id":"...","poll_secret":"..."}`. A 202 response with `{"status":"pending"}` means poll again after a short delay (not a long-held request). On an approved **first** sign-in, the poll returns `422 profile_required`; resend with `username` and `time_zone` to create the account and receive `{"status":"signed_in","token":"..."}`. Existing accounts need no profile. A deletion-pending account instead receives `restore_required` and a one-use `recovery_ticket`; after explicit consent, `POST /v1/auth/telegram/restore` accepts that ticket and returns a token. Attempts expire after 10 minutes. Poll secrets, recovery tickets, and session tokens must stay in the TUI, not the browser URL or logs. This flow signs into a Telegram identity; it does not automatically merge accounts that happen to share a name or email.

API errors use a stable JSON envelope, for example `{"error":{"code":"unauthorized","message":"Authentication required."}}`. Clients should branch on `code` and HTTP status, not message text. Shared codes and safe messages live in `internal/apierror`; internal database or server details stay in logs. A 401 includes `WWW-Authenticate`, and a 405 includes `Allow`.

The current protected and login routes have a 10-second request deadline; the server also limits header reading to 5 seconds, request reading to 10 seconds, responses to 30 seconds, and idle connections to 60 seconds. The Telegram poll endpoint is a short request, not long-polling. Future long-polling endpoints must explicitly choose compatible deadlines. Identity resolution and session issuance stay private to the auth package and are reachable only after verified OTP or OIDC completion.
