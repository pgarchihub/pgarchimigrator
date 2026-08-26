package auth

import (
	"context"
	"net/http"
	"time"
)

type contextKey string

const userContextKey contextKey = "auth_user"

// UserFromContext retrieves the authenticated User that RequireAuth placed
// on the request context, or nil if the request was never authenticated
// (e.g. a public route, or middleware wasn't applied).
func UserFromContext(ctx context.Context) *User {
	user, _ := ctx.Value(userContextKey).(*User)
	return user
}

// sessionCookieName is the name of the httpOnly cookie carrying the raw
// session token.
const sessionCookieName = "pgarchimigrator_session"

// RequireAuth wraps an http.Handler, rejecting any request without a
// valid session cookie (401) and otherwise injecting the authenticated
// User into the request context for downstream handlers (and RequireRole).
func RequireAuth(service *Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		user, err := service.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole wraps a handler that's already behind RequireAuth, further
// rejecting (403) any authenticated user whose role doesn't satisfy the
// minimum required role (see Role.Satisfies).
func RequireRole(minRole Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil || !user.Role.Satisfies(minRole) {
			writeAuthError(w, http.StatusForbidden, "insufficient permissions")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
}

// SetSessionCookie writes the session cookie for a newly-logged-in user.
// secure should be true whenever the server is reached over HTTPS (i.e.
// essentially always outside of local http://localhost development) —
// hardcoding false would let the cookie be sent over an unencrypted
// connection, undermining the whole point of an httpOnly, otherwise
// well-protected token.
func SetSessionCookie(w http.ResponseWriter, rawToken string, expiresAt time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    rawToken,
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
	})
}

// ClearSessionCookie expires the session cookie client-side — pair with a
// server-side Service.Logout call so the session is invalidated on both ends.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
	})
}

// SessionCookieValue extracts the raw session token from a request, or ""
// if there is none — used by the logout handler, which needs the raw
// token to invalidate the corresponding server-side session.
func SessionCookieValue(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}
