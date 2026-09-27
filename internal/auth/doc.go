// Package auth owns account identity and session lifecycle operations.
//
// Only a successful, provider-verified authentication result may resolve or
// create an account and issue a session. These helpers are private to auth;
// the email OTP completion path verifies the code before calling them.
// TODO(auth-boundary): Apply the same verified-result-only pattern when adding
// Telegram OIDC. Do not accept an unverified provider subject or account ID
// from an HTTP request. Handle deletion-pending accounts with explicit consent
// before restoring them or issuing a new session.
package auth
