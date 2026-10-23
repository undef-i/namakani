package tuta

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"golang.org/x/crypto/argon2"
)

const (
	argon2Iterations  = 4
	argon2MemoryKiB   = 32 * 1024
	argon2Parallelism = 1
	argon2KeyLen      = 32
	bcryptLogRounds   = 8
)

func passphraseKeyArgon2id(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, argon2Iterations, argon2MemoryKiB, argon2Parallelism, argon2KeyLen)
}

var bcryptMagic = []byte("OrpheanBeholderScryDoubt")

var errUnsupportedKeyLength = errors.New("tuta: unsupported key length")

func passphraseKeyBcrypt(passphrase string, salt []byte, bits int) ([]byte, error) {
	digest := sha256.Sum256([]byte(passphrase))
	encrypted := bcryptCryptRaw(digest[:], salt, bcryptLogRounds)
	switch bits {
	case 128:
		return encrypted[:16], nil
	case 256:
		sum := sha256.Sum256(encrypted)
		return sum[:], nil
	}
	return nil, errUnsupportedKeyLength
}

func authVerifier(passphraseKey []byte) string {
	sum := sha256.Sum256(passphraseKey)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
