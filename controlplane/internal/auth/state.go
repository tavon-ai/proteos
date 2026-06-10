package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// stateTTL bounds how long an OAuth state token is valid.
const stateTTL = 10 * time.Minute

var errBadState = errors.New("invalid or expired state")

// newState mints an opaque, HMAC-signed state value carrying a random nonce and
// an expiry. Format: base64(nonce).expiryUnix.base64(hmac).
func newState(key []byte) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	exp := time.Now().Add(stateTTL).Unix()
	payload := base64.RawURLEncoding.EncodeToString(nonce) + "." + strconv.FormatInt(exp, 10)
	sig := sign(key, payload)
	return payload + "." + sig, nil
}

// validateState verifies the signature (constant-time) and expiry of state.
func validateState(key []byte, state string) error {
	parts := strings.Split(state, ".")
	if len(parts) != 3 {
		return errBadState
	}
	payload := parts[0] + "." + parts[1]
	expected := sign(key, payload)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[2])) != 1 {
		return errBadState
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return errBadState
	}
	if time.Now().Unix() > exp {
		return errBadState
	}
	return nil
}

func sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
