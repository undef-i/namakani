package smcrypt

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func AppSalt(dataDir, privateDataName string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "SALT.php"))
	if err != nil {
		return "", fmt.Errorf("read SALT.php: %w", err)
	}
	salt := strings.TrimSpace(string(raw))
	sum := md5.Sum([]byte(salt + privateDataName + salt))
	return hex.EncodeToString(sum[:]), nil
}

func passphrase(key []byte, appSalt string) []byte {
	h := sha1.New()
	h.Write(key)
	h.Write([]byte(appSalt))
	return h.Sum(nil)
}

func mx(z, y, sum, k uint32) uint32 {
	return ((z>>5 ^ y<<2) + (y>>3 ^ z<<4)) ^ ((sum ^ y) + (k ^ z))
}

func str2long(s []byte, withLen bool) []uint32 {
	pad := (4 - len(s)%4) & 3
	b := make([]byte, len(s)+pad)
	copy(b, s)
	v := make([]uint32, len(b)/4)
	for i := range v {
		v[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	if withLen {
		v = append(v, uint32(len(s)))
	}
	return v
}

func long2str(v []uint32, withLen bool) ([]byte, error) {
	n := (len(v) - 1) << 2
	if withLen {
		m := int(v[len(v)-1])
		if m < n-3 || m > n {
			return nil, fmt.Errorf("xxtea: bad length word %d (block %d)", m, n)
		}
		n = m
	}
	out := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], x)
	}
	return out[:n], nil
}

func xxteaDecrypt(data, key []byte) ([]byte, error) {
	v := str2long(data, false)
	k := str2long(key, false)
	for len(k) < 4 {
		k = append(k, 0)
	}
	n := len(v) - 1
	if n < 1 {
		return nil, fmt.Errorf("xxtea: ciphertext too short")
	}
	const delta = 0x9E3779B9
	q := 6 + 52/(n+1)
	sum := uint32(q) * delta
	z := v[n]
	y := v[0]
	for sum != 0 {
		e := (sum >> 2) & 3
		for p := n; p > 0; p-- {
			z = v[p-1]
			y = v[p] - mx(z, y, sum, k[(uint32(p)&3)^e])
			v[p] = y
		}
		z = v[n]
		y = v[0] - mx(z, y, sum, k[e])
		v[0] = y
		sum -= delta
	}
	return long2str(v, true)
}

func decryptParts(parts [][]byte, key []byte, appSalt string) (any, error) {
	if len(parts) != 3 {
		return nil, fmt.Errorf("smcrypt: want 3 parts, got %d", len(parts))
	}
	if string(parts[0]) != "xxtea" {
		return nil, fmt.Errorf("smcrypt: unsupported cipher %q (only xxtea)", parts[0])
	}
	fullKey := make([]byte, 0, len(parts[1])+sha1.Size)
	fullKey = append(fullKey, parts[1]...)
	fullKey = append(fullKey, passphrase(key, appSalt)...)
	plain, err := xxteaDecrypt(parts[2], fullKey)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(plain, &v); err != nil {
		return nil, fmt.Errorf("smcrypt: plaintext is not JSON (wrong key?): %w", err)
	}
	return v, nil
}

func DecryptFromJSON(s string, key []byte, appSalt string) (any, error) {
	var arr []string
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return nil, fmt.Errorf("smcrypt: bad JSON envelope: %w", err)
	}
	if len(arr) != 3 {
		return nil, fmt.Errorf("smcrypt: want 3 envelope parts, got %d", len(arr))
	}
	parts := make([][]byte, 3)
	for i, p := range arr {
		b, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return nil, fmt.Errorf("smcrypt: envelope part %d: %w", i, err)
		}
		parts[i] = b
	}
	return decryptParts(parts, key, appSalt)
}

func urlSafeB64Decode(s string) ([]byte, error) {
	s = strings.NewReplacer("-", "+", "_", "/").Replace(s)
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(s)
}

func DecryptUrlSafe(s string, key []byte, appSalt string) (any, error) {
	fields := strings.Split(s, ".")
	if len(fields) != 3 {
		return nil, fmt.Errorf("smcrypt: want 3 urlsafe parts, got %d", len(fields))
	}
	parts := make([][]byte, 3)
	for i, f := range fields {
		b, err := urlSafeB64Decode(f)
		if err != nil {
			return nil, fmt.Errorf("smcrypt: urlsafe part %d: %w", i, err)
		}
		parts[i] = b
	}
	return decryptParts(parts, key, appSalt)
}

func VerifyHMAC(cryptKey []byte, token, wantHex string) bool {
	m := hmac.New(sha1.New, cryptKey)
	m.Write([]byte(token))
	return hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(wantHex))
}
