package tuta

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"

	"lukechampine.com/blake3"
)

const unitSeparator = "\x1f"

type typeID struct {
	app string
	id  int
}

func (t typeID) String() string { return fmt.Sprintf("%s/%d", t.app, t.id) }

func blake3KDF(ikm []byte, context string, length int) []byte {
	out := make([]byte, length)
	blake3.DeriveKey(out, context, ikm)
	return out
}

func blake3MAC(key, data []byte) []byte {
	h := blake3.New(32, key)
	h.Write(data)
	return h.Sum(nil)
}

type aeadSubKeys struct {
	encKey []byte
	macKey []byte
}

func deriveSubKeysFromSessionKey(sessionKey []byte, instType typeID) aeadSubKeys {
	derived := blake3KDF(sessionKey, "SK instanceSessionKey"+unitSeparator+instType.String(), 64)
	return aeadSubKeys{encKey: derived[:32], macKey: derived[32:]}
}

func deriveSubKeysFromGroupKey(groupKey, kdfNonce []byte, instType typeID) aeadSubKeys {
	ikm := make([]byte, 0, len(groupKey)+len(kdfNonce))
	ikm = append(ikm, groupKey...)
	ikm = append(ikm, kdfNonce...)
	derived := blake3KDF(ikm, "GK and nonce instanceMessageKey"+unitSeparator+instType.String(), 64)
	return aeadSubKeys{encKey: derived[:32], macKey: derived[32:]}
}

func cbcSubKeysFromGroupKey(groupKey, kdfNonce []byte, instType typeID) (encKey, macKey []byte) {
	ikm := make([]byte, 0, len(groupKey)+len(kdfNonce))
	ikm = append(ikm, groupKey...)
	ikm = append(ikm, kdfNonce...)
	derived := blake3KDF(ikm, "GK and nonce instanceMessageKey"+unitSeparator+instType.String(), 64)
	return derived[:32], derived[32:]
}

func aeadPad(data []byte) []byte {
	paddingLength := 4 - len(data)%4
	out := make([]byte, len(data)+paddingLength)
	copy(out, data)
	out[len(data)] = 0x80
	return out
}

func aeadUnpad(data []byte) ([]byte, error) {
	for i := len(data) - 1; i >= 0 && i >= len(data)-4; i-- {
		if data[i] == 0x80 {
			return data[:i], nil
		}
		if data[i] != 0 {
			break
		}
	}
	return nil, errors.New("tuta: invalid aead padding")
}

func aeadEncrypt(sub aeadSubKeys, plaintext, associatedData []byte) (iv, ciphertext, tag []byte) {
	iv = randomBytes(aes.BlockSize)
	ciphertext, tag = aeadEncryptWithIV(sub, iv, plaintext, associatedData)
	return iv, ciphertext, tag
}

func aeadEncryptWithIV(sub aeadSubKeys, iv, plaintext, associatedData []byte) (ciphertext, tag []byte) {
	padded := aeadPad(plaintext)
	block, err := aes.NewCipher(sub.encKey)
	if err != nil {
		panic(err)
	}
	ciphertext = make([]byte, len(padded))
	cipher.NewCTR(block, iv).XORKeyStream(ciphertext, padded)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(iv)+len(ciphertext)))
	macInput := make([]byte, 0, 4+len(iv)+len(ciphertext)+len(associatedData))
	macInput = append(macInput, lenBuf[:]...)
	macInput = append(macInput, iv...)
	macInput = append(macInput, ciphertext...)
	macInput = append(macInput, associatedData...)
	tag = blake3MAC(sub.macKey, macInput)
	return ciphertext, tag
}

func aeadDecrypt(sub aeadSubKeys, p *parsedCiphertext, associatedData []byte) ([]byte, error) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(p.iv)+len(p.ciphertext)))
	macInput := make([]byte, 0, 4+len(p.iv)+len(p.ciphertext)+len(associatedData))
	macInput = append(macInput, lenBuf[:]...)
	macInput = append(macInput, p.iv...)
	macInput = append(macInput, p.ciphertext...)
	macInput = append(macInput, associatedData...)
	tag := blake3MAC(sub.macKey, macInput)
	if !bytes.Equal(tag, p.tag) {
		return nil, errors.New("tuta: aead mac verification failed")
	}
	block, err := aes.NewCipher(sub.encKey)
	if err != nil {
		panic(err)
	}
	padded := make([]byte, len(p.ciphertext))
	cipher.NewCTR(block, p.iv).XORKeyStream(padded, p.ciphertext)
	return aeadUnpad(padded)
}

func encryptValueAeadSessionKey(sessionKey []byte, instType typeID, fieldPath string, plaintext []byte) []byte {
	sub := deriveSubKeysFromSessionKey(sessionKey, instType)
	ad := []byte("attributeEncSK" + unitSeparator + fieldPath)
	iv, ct, tag := aeadEncrypt(sub, plaintext, ad)
	out := make([]byte, 0, 1+len(iv)+len(ct)+len(tag))
	out = append(out, 3)
	out = append(out, iv...)
	out = append(out, ct...)
	out = append(out, tag...)
	return out
}

func encryptKeyAeadSessionKey(sessionKey, keyToEncrypt []byte, instType typeID) []byte {
	sub := deriveSubKeysFromSessionKey(sessionKey, instType)
	ad := []byte("attributeEncSK" + unitSeparator)
	iv, ct, tag := aeadEncrypt(sub, keyToEncrypt, ad)
	out := make([]byte, 0, 1+len(iv)+len(ct)+len(tag))
	out = append(out, 3)
	out = append(out, iv...)
	out = append(out, ct...)
	out = append(out, tag...)
	return out
}

func encryptKeyAeadGroupKey(groupKey, kdfNonce, keyToEncrypt []byte, groupKeyVersion int, instType typeID) []byte {
	sub := deriveSubKeysFromGroupKey(groupKey, kdfNonce, instType)
	ad := []byte("attributeEncGK" + unitSeparator)
	iv, ct, tag := aeadEncrypt(sub, keyToEncrypt, ad)
	out := make([]byte, 0, 3+len(iv)+len(ct)+len(tag))
	out = append(out, 2, 0, byte(groupKeyVersion))
	out = append(out, iv...)
	out = append(out, ct...)
	out = append(out, tag...)
	return out
}
