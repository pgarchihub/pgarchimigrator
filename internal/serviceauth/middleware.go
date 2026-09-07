package serviceauth

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const clientContextKey contextKey = "serviceauth_client"
const scopesContextKey contextKey = "serviceauth_scopes"

// ClientFromContext retrieves the authenticated Client that RequireBearer
// placed on the request context, or nil if the request was never
// authenticated this way (e.g. a route behind cookie-based auth.RequireAuth
// instead, or middleware wasn't applied) — mirrors
// auth.UserFromContext exactly.
func ClientFromContext(ctx context.Context) *Client {
	client, _ := ctx.Value(clientContextKey).(*Client)
	return client
}

// ScopesFromContext retrieves the scopes RequireBearer granted for THIS
// request's token (see AccessToken.Scopes' own doc comment for why this
// can be a subset of the client's full Scopes) — nil if the request
// wasn't authenticated via RequireBearer.
func ScopesFromContext(ctx context.Context) []string {
	scopes, _ := ctx.Value(scopesContextKey).([]string)
	return scopes
}

// RequireBearer wraps an http.Handler, rejecting any request without a
// valid "Authorization: Bearer <token>" header (401) and otherwise
// injecting the authenticated Client and its granted scopes into the
// request context for downstream handlers (and RequireScope) — the
// Bearer-token equivalent of auth.RequireAuth's session-cookie check.
// This is deliberately a SEPARATE middleware, not a modification to
// RequireAuth: a route protected by RequireBearer accepts service
// credentials, never a human session cookie, and vice versa — see
// docs/ecosystem/ARCHITECTURE.md for why these two authentication
// models are kept side by side rather than merged into one check that
// accepts either.
func RequireBearer(service *Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			// Service-to-service auth was never configured for this
			// instance — a distinct, explicit signal from "no token
			// provided" below, matching internal/api's own
			// handleOAuthToken precedent for the identical situation
			// (see that handler's own doc comment): a caller learns
			// this endpoint EXISTS but isn't enabled, not that its own
			// credentials are somehow wrong.
			writeBearerError(w, http.StatusServiceUnavailable, "invalid_client", "service-to-service authentication is not configured on this instance")
			return
		}
		token := bearerToken(r)
		if token == "" {
			writeBearerError(w, http.StatusUnauthorized, "invalid_client", "a Bearer access token is required")
			return
		}
		client, scopes, err := service.Authenticate(r.Context(), token)
		if err != nil {
			writeBearerError(w, http.StatusUnauthorized, "invalid_client", "invalid or expired access token")
			return
		}
		ctx := context.WithValue(r.Context(), clientContextKey, client)
		ctx = context.WithValue(ctx, scopesContextKey, scopes)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireScope wraps a handler that's already behind RequireBearer,
// further rejecting (403) any request whose token doesn't carry the
// given scope — the scope-based equivalent of auth.RequireRole's
// minimum-role check.
func RequireScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scopes := ScopesFromContext(r.Context())
		if !hasScope(scopes, scope) {
			writeBearerError(w, http.StatusForbidden, "insufficient_scope", "this token does not carry the required scope: "+scope)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasScope(granted []string, want string) bool {
	for _, s := range granted {
		if s == want {
			return true
		}
	}
	return false
}

// bearerToken extracts the raw token from an "Authorization: Bearer
// <token>" header — RFC 6750 Section 2.1. Returns "" for a missing
// header, a header using a different scheme, or a malformed one.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix))
}

// writeBearerError writes an RFC 6750 Section 3-shaped WWW-Authenticate
// header alongside a JSON body — a Bearer-token consumer (this being a
// machine caller, not a browser rendering an error page) is expected to
// parse either.
func writeBearerError(w http.ResponseWriter, status int, oauthErrorCode, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="`+oauthErrorCode+`", error_description="`+message+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + oauthErrorCode + `","error_description":"` + message + `"}`))
}
