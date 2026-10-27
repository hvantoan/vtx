package application

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters per OWASP's current minimum for password storage.
// Changing any of these is a hash-format change: verifyPassword reads the
// parameters from the stored PHC string, so old hashes keep working until the
// password is next set.
type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
	saltLen int
	keyLen  uint32
}

var defaultArgonParams = argonParams{memory: 64 * 1024, time: 3, threads: 2, saltLen: 16, keyLen: 32}

const passwordHashPrefix = "argon2id"

// dummyCredentialHash is verified for unknown usernames so a login attempt
// costs the same whether or not the account exists. The salt is fixed because
// this hash never protects anything; only its cost matters.
var dummyCredentialHash = encodePasswordHash([]byte("vtx-dummy-credential"), make([]byte, defaultArgonParams.saltLen))

func hashPassword(password string) (string, error) {
	salt := make([]byte, defaultArgonParams.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	return encodePasswordHash([]byte(password), salt), nil
}

func encodePasswordHash(password, salt []byte) string {
	key := argon2.IDKey(password, salt, defaultArgonParams.time, defaultArgonParams.memory, defaultArgonParams.threads, defaultArgonParams.keyLen)
	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		passwordHashPrefix,
		argon2.Version,
		defaultArgonParams.memory,
		defaultArgonParams.time,
		defaultArgonParams.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// verifyPassword compares a candidate against a PHC-encoded argon2id hash.
// It returns an error only for a malformed stored hash; a wrong password is
// (false, nil).
func verifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != passwordHashPrefix {
		return false, fmt.Errorf("malformed password hash: not argon2id")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("malformed password hash: version %q", parts[2])
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false, fmt.Errorf("malformed password hash: params %q", parts[3])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("malformed password hash: salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("malformed password hash: key: %w", err)
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
