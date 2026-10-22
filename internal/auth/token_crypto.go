package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// #nosec G101 -- HKDF domain-separation context string, not a credential
const tokenCipherHKDFInfo = "minicrm-session-refresh-token-v1"

// TokenCipher encrypts and decrypts OAuth2 refresh tokens stored in server-side sessions
// using AES-256-GCM bound to the owning user's UUID as Additional Authenticated Data (AAD).
type TokenCipher struct {
	aead cipher.AEAD
}

// NewTokenCipher derives a 32-byte AES-256-GCM key using HKDF-SHA256 from the first
// non-empty secret in secretMaterial. When all provided secrets are empty (e.g. in unit
// tests), an ephemeral random key is generated.
func NewTokenCipher(secretMaterial ...string) (*TokenCipher, error) {
	var rawSecret []byte
	for _, s := range secretMaterial {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			rawSecret = []byte(trimmed)
			break
		}
	}
	if len(rawSecret) == 0 {
		rawSecret = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, rawSecret); err != nil {
			return nil, fmt.Errorf("generate ephemeral session encryption key: %w", err)
		}
	}

	key, err := hkdf.Key(sha256.New, rawSecret, nil, tokenCipherHKDFInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("derive session encryption key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init aes-gcm: %w", err)
	}
	return &TokenCipher{aead: aead}, nil
}

// Encrypt encrypts plaintext using AES-256-GCM with a random nonce and userID as AAD.
func (c *TokenCipher) Encrypt(plaintext string, userID uuid.UUID) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("token cipher is not initialized")
	}
	if plaintext == "" {
		return nil, errors.New("cannot encrypt empty token")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), userID[:])
	return sealed, nil
}

// Decrypt decrypts and authenticates a ciphertext produced by Encrypt for the given userID.
func (c *TokenCipher) Decrypt(ciphertext []byte, userID uuid.UUID) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("token cipher is not initialized")
	}
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) <= nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plain, err := c.aead.Open(nil, nonce, sealed, userID[:])
	if err != nil {
		return "", fmt.Errorf("decrypt refresh token: %w", err)
	}
	return string(plain), nil
}

// ExtractRefreshTokenExpiry inspects an oauth2.Token and raw refresh_token string to determine
// the refresh token's expiration time. It checks:
//  1. "refresh_token_expires_in" and "refresh_expires_in" fields in the OAuth2 token response.
//  2. The "exp" claim if the refresh token is formatted as a JWT (e.g. Kanidm, Keycloak, Auth0).
func ExtractRefreshTokenExpiry(token *oauth2.Token, refreshToken string, now time.Time) (time.Time, bool) {
	if token != nil {
		for _, field := range []string{"refresh_token_expires_in", "refresh_expires_in"} {
			if secs, ok := parsePositiveSeconds(token.Extra(field)); ok {
				return now.Add(time.Duration(secs) * time.Second).UTC(), true
			}
		}
	}
	if exp, ok := parseJWTExpiry(refreshToken); ok && exp.After(now) {
		return exp.UTC(), true
	}
	return time.Time{}, false
}

func parsePositiveSeconds(raw any) (int64, bool) {
	switch v := raw.(type) {
	case float64:
		if v > 0 {
			return int64(v), true
		}
	case int64:
		if v > 0 {
			return v, true
		}
	case int:
		if v > 0 {
			return int64(v), true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return n, true
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func parseJWTExpiry(rawJWT string) (time.Time, bool) {
	parts := strings.Split(rawJWT, ".")
	if len(parts) != 3 || parts[1] == "" {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil || claims.Exp == "" {
		return time.Time{}, false
	}
	if sec, err := claims.Exp.Int64(); err == nil && sec > 0 {
		return time.Unix(sec, 0).UTC(), true
	}
	if f, err := claims.Exp.Float64(); err == nil && f > 0 {
		return time.Unix(int64(f), 0).UTC(), true
	}
	return time.Time{}, false
}
