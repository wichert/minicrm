// Package devidm provides a lightweight, standards-compliant dummy OAuth2/OIDC identity server
// with PKCE (S256) and RS256 JWT signing for local development and end-to-end testing.
package devidm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Config holds the configuration for the dummy OIDC identity server.
type Config struct {
	IssuerURL string
	ClientID  string
	// ClientSecret enables confidential-client authentication when set. When empty the
	// server emulates a public client: discovery advertises token endpoint auth method
	// "none" and any client credentials sent to /token are rejected.
	ClientSecret string
	Subject      string
	Email        string
	Name         string
	// Groups are emitted in the ID token under the "groups" claim.
	Groups []string
	// IDTokenClaimsOnly limits the ID token to standard claims (no email/name/groups),
	// like Zitadel without "User Info inside ID Token". UserInfo then serves them.
	IDTokenClaimsOnly bool
}

type authCodeEntry struct {
	clientID      string
	redirectURI   string
	nonce         string
	codeChallenge string
	expiresAt     time.Time
}

// Server is a minimal OIDC Identity Provider that automatically authenticates requests
// using the configured fixed user identity.
type Server struct {
	mu        sync.Mutex
	cfg       Config
	privKey   *rsa.PrivateKey
	pubJWKS   jose.JSONWebKeySet
	signer    jose.Signer
	authCodes map[string]authCodeEntry
}

// NewServer creates a new dummy OIDC Server with a freshly generated 2048-bit RSA signing key.
func NewServer(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Subject) == "" {
		cfg.Subject = "dev-admin-sub"
	}
	if strings.TrimSpace(cfg.Email) == "" {
		cfg.Email = "admin@example.com"
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = "Dev Admin"
	}
	cfg.IssuerURL = strings.TrimRight(strings.TrimSpace(cfg.IssuerURL), "/")

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate rsa key: %w", err)
	}
	keyID := "dev-key-1"
	pubJWK := jose.JSONWebKey{
		Key:       &privKey.PublicKey,
		KeyID:     keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	privJWK := jose.JSONWebKey{
		Key:       privKey,
		KeyID:     keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privJWK},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID),
	)
	if err != nil {
		return nil, fmt.Errorf("create jwt signer: %w", err)
	}

	return &Server{
		cfg:       cfg,
		privKey:   privKey,
		pubJWKS:   jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pubJWK}},
		signer:    signer,
		authCodes: make(map[string]authCodeEntry),
	}, nil
}

// SetIssuerURL sets or updates the issuer URL (useful when binding to an ephemeral test port).
func (s *Server) SetIssuerURL(issuerURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.IssuerURL = strings.TrimRight(strings.TrimSpace(issuerURL), "/")
}

// IssuerURL returns the configured OIDC issuer URL.
func (s *Server) IssuerURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.IssuerURL
}

// Handler returns an http.Handler serving the OIDC Discovery, JWKS, authorize, and token endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /jwks", s.handleJWKS)
	mux.HandleFunc("GET /authorize", s.handleAuthorize)
	mux.HandleFunc("POST /token", s.handleToken)
	mux.HandleFunc("GET /userinfo", s.handleUserInfo)
	return mux
}

func (s *Server) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	issuer := s.IssuerURL()
	authMethods := []string{"none"}
	if s.cfg.ClientSecret != "" {
		authMethods = []string{"client_secret_basic", "client_secret_post"}
	}
	doc := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"jwks_uri":                              issuer + "/jwks",
		"userinfo_endpoint":                     issuer + "/userinfo",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email", "groups"},
		"claims_supported":                      []string{"iss", "sub", "aud", "iat", "exp", "nonce", "email", "email_verified", "name", "groups"},
		"token_endpoint_auth_methods_supported": authMethods,
		"code_challenge_methods_supported":      []string{"S256"},
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.pubJWKS)
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := strings.TrimSpace(q.Get("client_id"))
	redirectURI := strings.TrimSpace(q.Get("redirect_uri"))
	state := q.Get("state")
	nonce := q.Get("nonce")
	codeChallenge := strings.TrimSpace(q.Get("code_challenge"))
	challengeMethod := strings.TrimSpace(q.Get("code_challenge_method"))

	if clientID == "" || redirectURI == "" {
		http.Error(w, "missing client_id or redirect_uri", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	expectedClient := s.cfg.ClientID
	s.mu.Unlock()
	if expectedClient != "" && clientID != expectedClient {
		http.Error(w, "unauthorized client_id", http.StatusBadRequest)
		return
	}

	parsedRedirect, err := url.Parse(redirectURI)
	if err != nil || (parsedRedirect.Scheme != "http" && parsedRedirect.Scheme != "https") || parsedRedirect.Host == "" {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	if codeChallenge != "" && challengeMethod != "" && challengeMethod != "S256" {
		http.Error(w, "unsupported code_challenge_method (want S256)", http.StatusBadRequest)
		return
	}

	code, err := randomCode()
	if err != nil {
		http.Error(w, "failed to generate code", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.authCodes[code] = authCodeEntry{
		clientID:      clientID,
		redirectURI:   redirectURI,
		nonce:         nonce,
		codeChallenge: codeChallenge,
		expiresAt:     time.Now().Add(5 * time.Minute),
	}
	s.mu.Unlock()

	qOut := parsedRedirect.Query()
	qOut.Set("code", code)
	if state != "" {
		qOut.Set("state", state)
	}
	parsedRedirect.RawQuery = qOut.Encode()
	// #nosec G710 -- OIDC /authorize redirects back to the client's validated http/https redirect_uri
	http.Redirect(w, r, parsedRedirect.String(), http.StatusFound)
}

type idTokenClaims struct {
	jwt.Claims
	Nonce         string   `json:"nonce,omitempty"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name"`
	Groups        []string `json:"groups,omitempty"`
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if r.FormValue("grant_type") != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	codeVerifier := strings.TrimSpace(r.FormValue("code_verifier"))
	clientID, clientSecret, hasBasic := r.BasicAuth()
	if !hasBasic {
		clientID = strings.TrimSpace(r.FormValue("client_id"))
		clientSecret = r.FormValue("client_secret")
	}

	if s.cfg.ClientSecret == "" && (hasBasic || clientSecret != "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":             "invalid_client",
			"error_description": "public client (token_endpoint_auth_method none) must not send client credentials",
		})
		return
	}

	entry, cfg, err := s.consumeAuthCode(code, clientID, clientSecret, codeVerifier)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":             "invalid_grant",
			"error_description": err.Error(),
		})
		return
	}

	now := time.Now().UTC()
	audClientID := entry.clientID
	if clientID != "" {
		audClientID = clientID
	}
	claims := idTokenClaims{
		Claims: jwt.Claims{
			Issuer:    cfg.IssuerURL,
			Subject:   cfg.Subject,
			Audience:  jwt.Audience{audClientID},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			Expiry:    jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Nonce:         entry.nonce,
		Email:         cfg.Email,
		EmailVerified: true,
		Name:          cfg.Name,
		Groups:        cfg.Groups,
	}
	if cfg.IDTokenClaimsOnly {
		claims.Email = ""
		claims.Name = ""
		claims.Groups = nil
	}

	rawIDToken, err := jwt.Signed(s.signer).Claims(claims).Serialize()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "dev-access-token-" + code,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     rawIDToken,
	})
}

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") || !strings.HasPrefix(strings.TrimPrefix(authz, "Bearer "), "dev-access-token-") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"sub":            cfg.Subject,
		"email":          cfg.Email,
		"email_verified": true,
		"name":           cfg.Name,
		"groups":         cfg.Groups,
	})
}

func (s *Server) consumeAuthCode(code, clientID, clientSecret, codeVerifier string) (authCodeEntry, Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cfg.ClientSecret != "" && clientSecret != s.cfg.ClientSecret {
		return authCodeEntry{}, Config{}, errors.New("invalid client_secret")
	}
	entry, ok := s.authCodes[code]
	if !ok {
		return authCodeEntry{}, Config{}, errors.New("unknown or already used authorization code")
	}
	delete(s.authCodes, code)

	if time.Now().After(entry.expiresAt) {
		return authCodeEntry{}, Config{}, errors.New("authorization code expired")
	}
	if clientID != "" && entry.clientID != clientID {
		return authCodeEntry{}, Config{}, errors.New("client_id mismatch")
	}
	if entry.codeChallenge != "" {
		if codeVerifier == "" {
			return authCodeEntry{}, Config{}, errors.New("missing pkce code_verifier")
		}
		sum := sha256.Sum256([]byte(codeVerifier))
		computed := base64.RawURLEncoding.EncodeToString(sum[:])
		if computed != entry.codeChallenge {
			return authCodeEntry{}, Config{}, errors.New("pkce code_verifier mismatch")
		}
	}
	return entry, s.cfg, nil
}

func randomCode() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
