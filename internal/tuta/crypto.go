package tuta

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
)

var fixedIV = []byte{
	0x88, 0x88, 0x88, 0x88, 0x88, 0x88, 0x88, 0x88,
	0x88, 0x88, 0x88, 0x88, 0x88, 0x88, 0x88, 0x88,
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	n := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+n)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(n)
	}
	return out
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("tuta: invalid padded length")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > blockSize || n > len(data) {
		return nil, errors.New("tuta: invalid padding")
	}
	for i := len(data) - n; i < len(data); i++ {
		if int(data[i]) != n {
			return nil, errors.New("tuta: invalid padding")
		}
	}
	return data[:len(data)-n], nil
}

func aesCBCEncryptRaw(key, iv, data []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
	return out
}

func aesCBCDecryptRaw(key, iv, data []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	return out
}

func aesCBCEncrypt(key, iv, plaintext []byte) []byte {
	return aesCBCEncryptRaw(key, iv, pkcs7Pad(plaintext, aes.BlockSize))
}

func aesCBCDecrypt(key, iv, ciphertext []byte) ([]byte, error) {
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("tuta: ciphertext not a multiple of the block size")
	}
	return pkcs7Unpad(aesCBCDecryptRaw(key, iv, ciphertext), aes.BlockSize)
}

type cipherKind int

const (
	kindLegacy cipherKind = iota
	kindCBCUnauthenticated
	kindCBCWithMAC
	kindAeadGroupKey
	kindAeadSessionKey
)

type parsedCiphertext struct {
	kind            cipherKind
	groupKeyVersion byte
	iv              []byte
	ciphertext      []byte
	tag             []byte
}

func parseCiphertext(data []byte) (*parsedCiphertext, error) {
	if len(data)%2 == 0 {
		if len(data) < aes.BlockSize*2 {
			return nil, errors.New("tuta: legacy ciphertext too short")
		}
		return &parsedCiphertext{
			kind:       kindLegacy,
			iv:         data[:aes.BlockSize],
			ciphertext: data[aes.BlockSize:],
		}, nil
	}
	if len(data) < 1 {
		return nil, errors.New("tuta: empty ciphertext")
	}
	switch data[0] {
	case 0:
		if len(data) < 1+aes.BlockSize {
			return nil, errors.New("tuta: cbc ciphertext too short")
		}
		return &parsedCiphertext{
			kind:       kindCBCUnauthenticated,
			iv:         data[1 : 1+aes.BlockSize],
			ciphertext: data[1+aes.BlockSize:],
		}, nil
	case 1:
		if len(data) < 1+aes.BlockSize+sha256.Size {
			return nil, errors.New("tuta: cbc-mac ciphertext too short")
		}
		return &parsedCiphertext{
			kind:       kindCBCWithMAC,
			iv:         data[1 : 1+aes.BlockSize],
			ciphertext: data[1+aes.BlockSize : len(data)-sha256.Size],
			tag:        data[len(data)-sha256.Size:],
		}, nil
	case 2:
		if len(data) < 3+aes.BlockSize+sha256.Size {
			return nil, errors.New("tuta: aead ciphertext too short")
		}
		if data[1] != 0 {
			return nil, fmt.Errorf("tuta: unsupported aead key identifier %d", data[1])
		}
		return &parsedCiphertext{
			kind:            kindAeadGroupKey,
			groupKeyVersion: data[2],
			iv:              data[3 : 3+aes.BlockSize],
			ciphertext:      data[3+aes.BlockSize : len(data)-sha256.Size],
			tag:             data[len(data)-sha256.Size:],
		}, nil
	case 3:
		if len(data) < 1+aes.BlockSize+sha256.Size {
			return nil, errors.New("tuta: aead ciphertext too short")
		}
		return &parsedCiphertext{
			kind:       kindAeadSessionKey,
			iv:         data[1 : 1+aes.BlockSize],
			ciphertext: data[1+aes.BlockSize : len(data)-sha256.Size],
			tag:        data[len(data)-sha256.Size:],
		}, nil
	}
	return nil, fmt.Errorf("tuta: unsupported cipher version %d", data[0])
}

func cbcSubKeys(key []byte) (encKey, macKey []byte) {
	switch len(key) {
	case 16:
		sum := sha256.Sum256(key)
		return sum[:16], sum[16:]
	case 32:
		sum := sha512.Sum512(key)
		return sum[:32], sum[32:]
	}
	panic("tuta: invalid key length")
}

func encryptCBCWithMAC(key, plaintext []byte) []byte {
	encKey, macKey := cbcSubKeys(key)
	iv := randomBytes(aes.BlockSize)
	ct := aesCBCEncrypt(encKey, iv, plaintext)
	mac := hmac.New(sha256.New, macKey)
	mac.Write(iv)
	mac.Write(ct)
	out := make([]byte, 0, 1+len(iv)+len(ct)+sha256.Size)
	out = append(out, 1)
	out = append(out, iv...)
	out = append(out, ct...)
	out = append(out, mac.Sum(nil)...)
	return out
}

func decryptCBCWithMAC(key []byte, p *parsedCiphertext) ([]byte, error) {
	encKey, macKey := cbcSubKeys(key)
	mac := hmac.New(sha256.New, macKey)
	mac.Write(p.iv)
	mac.Write(p.ciphertext)
	if !hmac.Equal(mac.Sum(nil), p.tag) {
		return nil, errors.New("tuta: mac verification failed")
	}
	return aesCBCDecrypt(encKey, p.iv, p.ciphertext)
}

func encryptBytes(key, plaintext []byte) []byte {
	if len(key) == 16 {
		iv := randomBytes(aes.BlockSize)
		ct := aesCBCEncrypt(key, iv, plaintext)
		out := make([]byte, 0, len(iv)+len(ct))
		out = append(out, iv...)
		out = append(out, ct...)
		return out
	}
	return encryptCBCWithMAC(key, plaintext)
}

func decryptBytes(key, data []byte) ([]byte, error) {
	p, err := parseCiphertext(data)
	if err != nil {
		return nil, err
	}
	switch p.kind {
	case kindLegacy, kindCBCUnauthenticated:
		return aesCBCDecrypt(key, p.iv, p.ciphertext)
	case kindCBCWithMAC:
		return decryptCBCWithMAC(key, p)
	}
	return nil, fmt.Errorf("tuta: cipher version %d not valid for byte decryption", data[0])
}

func encryptKey(encryptionKey, keyToEncrypt []byte) []byte {
	if len(encryptionKey) == 16 {
		return aesCBCEncryptRaw(encryptionKey, fixedIV, keyToEncrypt)
	}
	encKey, macKey := cbcSubKeys(encryptionKey)
	iv := randomBytes(aes.BlockSize)
	ct := aesCBCEncryptRaw(encKey, iv, keyToEncrypt)
	mac := hmac.New(sha256.New, macKey)
	mac.Write(iv)
	mac.Write(ct)
	out := make([]byte, 0, 1+len(iv)+len(ct)+sha256.Size)
	out = append(out, 1)
	out = append(out, iv...)
	out = append(out, ct...)
	out = append(out, mac.Sum(nil)...)
	return out
}

func decryptKey(encryptionKey, encryptedKey []byte) ([]byte, error) {
	if len(encryptionKey) == 16 {
		if len(encryptedKey)%aes.BlockSize != 0 {
			return nil, errors.New("tuta: encrypted key not a multiple of the block size")
		}
		return aesCBCDecryptRaw(encryptionKey, fixedIV, encryptedKey), nil
	}
	p, err := parseCiphertext(encryptedKey)
	if err != nil {
		return nil, err
	}
	switch p.kind {
	case kindLegacy, kindCBCUnauthenticated:
		return aesCBCDecryptRaw(encryptionKey, p.iv, p.ciphertext), nil
	case kindCBCWithMAC:
		encKey, macKey := cbcSubKeys(encryptionKey)
		mac := hmac.New(sha256.New, macKey)
		mac.Write(p.iv)
		mac.Write(p.ciphertext)
		if !hmac.Equal(mac.Sum(nil), p.tag) {
			return nil, errors.New("tuta: mac verification failed")
		}
		return aesCBCDecryptRaw(encKey, p.iv, p.ciphertext), nil
	}
	return nil, fmt.Errorf("tuta: cipher version %d not valid for key decryption", encryptedKey[0])
}
