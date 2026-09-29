// Package auth owns account identity and session lifecycle operations.
//
// Only a successful, provider-verified authentication result may resolve or
// create an account and issue a session. These helpers are private to auth;
// the email OTP and Telegram OIDC completion paths verify ownership before
// calling them. Do not accept an unverified provider subject or account ID
// from an HTTP request. Deletion-pending accounts require explicit consent
// before restoration, and cannot receive a normal login session.
package auth
