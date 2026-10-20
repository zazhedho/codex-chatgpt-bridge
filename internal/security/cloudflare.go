package security

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const accessJWKSMaxBytes = 1 << 20

type AccessAssertionVerifier interface {
	Verify(context.Context, string) (string, error)
}

type CloudflareAccessVerifier struct {
	issuer   string
	audience string
	allowed  map[string]struct{}
	client   *http.Client
	mu       sync.Mutex
	keys     map[string]*rsa.PublicKey
	fetched  time.Time
}

type accessClaims struct {
	jwt.RegisteredClaims
	Email string `json:"email"`
}

func NewCloudflareAccessVerifier(issuer, audience string, allowedEmails []string) (*CloudflareAccessVerifier, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || !validAccessIssuer(parsed) || audience == "" || len(allowedEmails) == 0 {
		return nil, errors.New("invalid Cloudflare Access verifier configuration")
	}
	allowed := make(map[string]struct{}, len(allowedEmails))
	for _, value := range allowedEmails {
		email := strings.ToLower(strings.TrimSpace(value))
		parsedEmail, err := mail.ParseAddress(email)
		if err != nil || email == "" || parsedEmail.Address != email {
			return nil, errors.New("invalid Cloudflare Access verifier configuration")
		}
		allowed[email] = struct{}{}
	}
	return &CloudflareAccessVerifier{
		issuer: issuer, audience: audience, allowed: allowed,
		client: &http.Client{Timeout: 5 * time.Second},
		keys:   make(map[string]*rsa.PublicKey),
	}, nil
}

func validAccessIssuer(issuer *url.URL) bool {
	if issuer == nil || issuer.Host == "" || issuer.User != nil || issuer.Path != "" || issuer.RawQuery != "" || issuer.Fragment != "" {
		return false
	}
	if issuer.Scheme == "https" {
		return true
	}
	if issuer.Scheme != "http" {
		return false
	}
	host := issuer.Hostname()
	ip := net.ParseIP(host)
	return (ip != nil && ip.IsLoopback()) || strings.EqualFold(host, "localhost")
}

func (v *CloudflareAccessVerifier) Verify(ctx context.Context, assertion string) (string, error) {
	if assertion == "" {
		return "", errors.New("invalid Cloudflare Access assertion")
	}
	claims := &accessClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience), jwt.WithExpirationRequired())
	token, err := parser.ParseWithClaims(assertion, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" || token.Method.Alg() != "RS256" {
			return nil, errors.New("invalid Cloudflare Access signing key")
		}
		return v.signingKey(ctx, kid)
	})
	if err != nil || token == nil || !token.Valid || claims.NotBefore == nil || strings.TrimSpace(claims.Subject) == "" {
		return "", errors.New("invalid Cloudflare Access assertion")
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if _, ok := v.allowed[email]; !ok || email == "" {
		return "", errors.New("invalid Cloudflare Access assertion")
	}
	return "cloudflare-access:" + email, nil
}

func (v *CloudflareAccessVerifier) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Since(v.fetched) < 5*time.Minute {
		if key := v.keys[kid]; key != nil {
			return key, nil
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(v.issuer, "/")+"/cdn-cgi/access/certs", nil)
	if err != nil {
		return nil, errors.New("Cloudflare Access signing keys unavailable")
	}
	response, err := v.client.Do(request)
	if err != nil {
		return nil, errors.New("Cloudflare Access signing keys unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("Cloudflare Access signing keys unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, accessJWKSMaxBytes+1))
	if err != nil || len(body) > accessJWKSMaxBytes {
		return nil, errors.New("Cloudflare Access signing keys unavailable")
	}
	keys, err := parseAccessJWKS(body)
	if err != nil {
		return nil, errors.New("Cloudflare Access signing keys unavailable")
	}
	v.keys = keys
	v.fetched = time.Now()
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, errors.New("Cloudflare Access signing key not found")
}

type accessJWKS struct {
	Keys []struct {
		Kty string `json:"kty"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func parseAccessJWKS(body []byte) (map[string]*rsa.PublicKey, error) {
	var document accessJWKS
	if err := json.Unmarshal(body, &document); err != nil || len(document.Keys) == 0 {
		return nil, errors.New("invalid Cloudflare Access signing keys")
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, value := range document.Keys {
		if value.Kty != "RSA" || value.Use != "sig" || value.Alg != "RS256" {
			continue
		}
		modulus, errN := base64.RawURLEncoding.DecodeString(value.N)
		exponent, errE := base64.RawURLEncoding.DecodeString(value.E)
		if value.Kid == "" || errN != nil || errE != nil || len(modulus) < 256 || len(exponent) == 0 {
			return nil, errors.New("invalid Cloudflare Access signing key")
		}
		n := new(big.Int).SetBytes(modulus)
		e := new(big.Int).SetBytes(exponent)
		if n.BitLen() < 2048 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > math.MaxInt || e.Int64()%2 == 0 {
			return nil, errors.New("invalid Cloudflare Access signing key")
		}
		if _, duplicate := keys[value.Kid]; duplicate {
			return nil, errors.New("duplicate Cloudflare Access key id")
		}
		keys[value.Kid] = &rsa.PublicKey{N: n, E: int(e.Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("no supported Cloudflare Access signing keys")
	}
	return keys, nil
}

func CloudflareAccessMiddlewareForHost(verifier AccessAssertionVerifier, expectedHost string, next http.Handler) http.Handler {
	expectedHost = strings.ToLower(expectedHost)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expectedHost == "" || !strings.EqualFold(r.Host, expectedHost) {
			writeHTTPError(w, http.StatusForbidden, "Forbidden")
			return
		}
		if verifier == nil {
			writeHTTPError(w, http.StatusServiceUnavailable, "Authentication unavailable")
			return
		}
		assertion := r.Header.Get("Cf-Access-Jwt-Assertion")
		identity, err := verifier.Verify(r.Context(), assertion)
		if err != nil || identity == "" {
			writeHTTPError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{id: identity})))
	})
}
