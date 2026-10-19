package security

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
)

type principalKey struct{}
type principal struct{ id string }

func Authorize(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(principalKey{}).(principal)
	return value.id, ok && value.id != ""
}

func AuthenticateBearer(header, expected string) bool {
	if len(expected) < 32 || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	supplied := strings.TrimPrefix(header, "Bearer ")
	expectedHash := sha256.Sum256([]byte(expected))
	suppliedHash := sha256.Sum256([]byte(supplied))
	return subtle.ConstantTimeCompare(expectedHash[:], suppliedHash[:]) == 1
}

func AuthMiddleware(token string, port int, next http.Handler) http.Handler {
	return AuthMiddlewareForHost(token, "127.0.0.1", port, next)
}

func AuthMiddlewareForHost(token, bindHost string, port int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, hostPort, err := net.SplitHostPort(r.Host)
		if err != nil || hostPort != strconv.Itoa(port) || !sameLoopbackHost(host, bindHost) {
			writeHTTPError(w, http.StatusForbidden, "Forbidden")
			return
		}
		if !AuthenticateBearer(r.Header.Get("Authorization"), token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeHTTPError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{id: "mcp-bearer"})))
	})
}

func sameLoopbackHost(requestHost, bindHost string) bool {
	requestIP, bindIP := net.ParseIP(requestHost), net.ParseIP(bindHost)
	return requestIP != nil && bindIP != nil && requestIP.IsLoopback() && bindIP.IsLoopback() && requestIP.Equal(bindIP)
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
