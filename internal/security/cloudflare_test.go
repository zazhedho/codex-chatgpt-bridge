package security

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudflareAccessMiddlewareRequiresVerifiedAssertionAndExactHost(t *testing.T) {
	key := testRSAKey(t)
	server := jwksTestServer(t, jwksFor(t, "key-1", &key.PublicKey))
	defer server.Close()
	verifier := testCloudflareVerifier(t, server, []string{"zaqi@example.com"})
	called := false
	handler := CloudflareAccessMiddlewareForHost(verifier, "mcp.example.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if id, ok := Authorize(r.Context()); !ok || id != "cloudflare-access:zaqi@example.com" {
			t.Errorf("principal = %q, ok=%v", id, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	localBearerOnly := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", nil)
	localBearerOnly.Host = "mcp.example.test"
	localBearerOnly.Header.Set("Authorization", "Bearer local-token-that-must-not-fallback")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, localBearerOnly)
	if response.Code != http.StatusUnauthorized || called {
		t.Fatalf("local bearer fallback status=%d called=%v, want 401 and no handler call", response.Code, called)
	}

	assertion := signAccessToken(t, key, "key-1", map[string]any{
		"iss": server.URL, "aud": "expected-audience", "sub": "user-1", "email": "zaqi@example.com",
		"exp": time.Now().Add(time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", nil)
	request.Host = "mcp.example.test"
	request.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("valid assertion status=%d called=%v, want 204 and handler call", response.Code, called)
	}

	request.Host = "attacker.example.test"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unexpected Host status=%d, want 403", response.Code)
	}
}

func TestCloudflareAccessVerifierRejectsInvalidClaimsAndKeys(t *testing.T) {
	key := testRSAKey(t)
	otherKey := testRSAKey(t)
	server := jwksTestServer(t, jwksFor(t, "key-1", &key.PublicKey))
	defer server.Close()
	verifier := testCloudflareVerifier(t, server, []string{"zaqi@example.com"})
	now := time.Now()
	validClaims := func() map[string]any {
		return map[string]any{
			"iss": server.URL, "aud": "expected-audience", "sub": "user-1", "email": "zaqi@example.com",
			"exp": now.Add(time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(),
		}
	}

	for _, test := range []struct {
		name  string
		key   *rsa.PrivateKey
		kid   string
		claim func() map[string]any
	}{
		{name: "invalid signature", key: otherKey, kid: "key-1", claim: validClaims},
		{name: "wrong issuer", key: key, kid: "key-1", claim: func() map[string]any { c := validClaims(); c["iss"] = "https://attacker.example"; return c }},
		{name: "wrong audience", key: key, kid: "key-1", claim: func() map[string]any { c := validClaims(); c["aud"] = "other-audience"; return c }},
		{name: "expired", key: key, kid: "key-1", claim: func() map[string]any { c := validClaims(); c["exp"] = now.Add(-time.Minute).Unix(); return c }},
		{name: "expired within clock skew", key: key, kid: "key-1", claim: func() map[string]any { c := validClaims(); c["exp"] = now.Add(-time.Second).Unix(); return c }},
		{name: "not yet valid", key: key, kid: "key-1", claim: func() map[string]any { c := validClaims(); c["nbf"] = now.Add(time.Minute).Unix(); return c }},
		{name: "not yet valid within clock skew", key: key, kid: "key-1", claim: func() map[string]any { c := validClaims(); c["nbf"] = now.Add(time.Second).Unix(); return c }},
		{name: "unknown key id", key: key, kid: "unknown", claim: validClaims},
		{name: "disallowed email", key: key, kid: "key-1", claim: func() map[string]any { c := validClaims(); c["email"] = "other@example.com"; return c }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertion := signAccessToken(t, test.key, test.kid, test.claim())
			if _, err := verifier.Verify(context.Background(), assertion); err == nil {
				t.Fatal("invalid Access assertion was accepted")
			}
		})
	}
}

func TestCloudflareAccessVerifierRefreshesRotatedSigningKey(t *testing.T) {
	firstKey, secondKey := testRSAKey(t), testRSAKey(t)
	current := atomic.Value{}
	current.Store(jwksFor(t, "key-1", &firstKey.PublicKey))
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(current.Load().([]byte))
	}))
	defer server.Close()
	verifier := testCloudflareVerifier(t, server, []string{"zaqi@example.com"})
	makeClaims := func() map[string]any {
		return map[string]any{
			"iss": server.URL, "aud": "expected-audience", "sub": "user-1", "email": "zaqi@example.com",
			"exp": time.Now().Add(time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
		}
	}
	if _, err := verifier.Verify(context.Background(), signAccessToken(t, firstKey, "key-1", makeClaims())); err != nil {
		t.Fatalf("initial key rejected: %v", err)
	}
	current.Store(jwksFor(t, "key-2", &secondKey.PublicKey))
	if _, err := verifier.Verify(context.Background(), signAccessToken(t, secondKey, "key-2", makeClaims())); err != nil {
		t.Fatalf("rotated key rejected: %v", err)
	}
	if requests.Load() < 2 {
		t.Fatalf("JWKS requests=%d, want refresh after unknown kid", requests.Load())
	}
}

func TestCloudflareAccessVerifierFailsClosedWithoutCachedKey(t *testing.T) {
	key := testRSAKey(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	verifier := testCloudflareVerifier(t, server, []string{"zaqi@example.com"})
	assertion := signAccessToken(t, key, "key-1", map[string]any{
		"iss": server.URL, "aud": "expected-audience", "sub": "user-1", "email": "zaqi@example.com",
		"exp": time.Now().Add(time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
	})
	if _, err := verifier.Verify(context.Background(), assertion); err == nil {
		t.Fatal("accepted an assertion when JWKS was unavailable and no key was cached")
	}
}

func testCloudflareVerifier(t *testing.T, server *httptest.Server, emails []string) *CloudflareAccessVerifier {
	t.Helper()
	verifier, err := NewCloudflareAccessVerifier(server.URL, "expected-audience", emails)
	if err != nil {
		t.Fatal(err)
	}
	verifier.client = server.Client()
	return verifier
}

func jwksTestServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/access/certs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func jwksFor(t *testing.T, kid string, publicKey *rsa.PublicKey) []byte {
	t.Helper()
	key := map[string]string{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
	}
	body, err := json.Marshal(map[string]any{"keys": []any{key}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func signAccessToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(encoded))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s.%s", encoded, base64.RawURLEncoding.EncodeToString(signature))
}
