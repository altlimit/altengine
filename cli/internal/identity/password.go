package identity

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

// Password hashing: PBKDF2-HMAC-SHA256 with a per-user random salt, stored as
// `pbkdf2$<iterations>$<salt>$<hash>` (both parts unpadded base64url). Verification is
// constant-time, and a miss still runs a full derivation so an unknown account and a wrong
// password cost the same (no timing oracle on which handles exist).

const (
	pbkdf2Iterations = 100_000
	pbkdf2KeyLen     = 32
	pbkdf2SaltLen    = 16
)

func b64raw(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// HashPassword derives a storable hash for a plaintext password.
func HashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return "pbkdf2$" + strconv.Itoa(pbkdf2Iterations) + "$" + b64raw(salt) + "$" + b64raw(key), nil
}

// VerifyPassword checks a plaintext password against a stored hash.
func VerifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > 1_000_000 {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// dummyHash is verified against when no such account exists, so a sign-in miss takes the
// same work as a wrong password.
var dummyHash, _ = HashPassword("altengine-emulator-dummy-password")

// hashSecret is the at-rest form of an opaque secret (refresh token, one-time code):
// only its SHA-256 is stored, so reading the database never yields a usable credential.
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
