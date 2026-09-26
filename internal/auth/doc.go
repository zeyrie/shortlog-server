// Package auth owns account identity and session lifecycle operations.
//
// TODO(auth-boundary): Before exposing email OTP or OIDC login endpoints,
// make ResolveVerifiedIdentity and IssueSession private to this package.
// Only a successful, provider-verified authentication result may resolve or
// create an account and issue a session. Do not accept a provider subject or
// account ID directly from an HTTP request as proof of identity. Login flows
// must also handle deletion-pending accounts with an explicit restore step.
package auth
