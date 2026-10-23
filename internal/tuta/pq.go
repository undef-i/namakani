package tuta

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/cloudflare/circl/kem/mlkem/mlkem1024"
	"golang.org/x/crypto/sha3"
)

const cryptoProtocolTutaCrypt = 2

func byteArraysToBytes(arrays [][]byte) []byte {
	var out []byte
	for _, a := range arrays {
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(a)))
		out = append(out, lb[:]...)
		out = append(out, a...)
	}
	return out
}

func bytesToByteArrays(data []byte, count int) ([][]byte, error) {
	out := make([][]byte, 0, count)
	rest := data
	for i := 0; i < count; i++ {
		if len(rest) < 2 {
			return nil, errors.New("tuta: byte array encoding too short")
		}
		l := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if len(rest) < l {
			return nil, errors.New("tuta: byte array part overruns input")
		}
		out = append(out, rest[:l])
		rest = rest[l:]
	}
	return out, nil
}

func kyberPublicKeyToRaw(encoded []byte) ([]byte, error) {
	parts, err := bytesToByteArrays(encoded, 2)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 0, len(parts[0])+len(parts[1]))
	raw = append(raw, parts[0]...)
	raw = append(raw, parts[1]...)
	return raw, nil
}

func kyberPublicKeyFromRaw(raw []byte) []byte {
	return byteArraysToBytes([][]byte{raw[:1536], raw[1536:]})
}

func kyberPrivateKeyToRaw(encoded []byte) ([]byte, error) {
	parts, err := bytesToByteArrays(encoded, 5)
	if err != nil {
		return nil, err
	}
	s, hpk, nonce, t, rho := parts[0], parts[1], parts[2], parts[3], parts[4]
	raw := make([]byte, 0, len(s)+len(t)+len(rho)+len(hpk)+len(nonce))
	raw = append(raw, s...)
	raw = append(raw, t...)
	raw = append(raw, rho...)
	raw = append(raw, hpk...)
	raw = append(raw, nonce...)
	return raw, nil
}

func kyberPrivateKeyFromRaw(raw []byte) []byte {
	s := raw[:1536]
	t := raw[1536:3072]
	rho := raw[3072:3104]
	hpk := raw[3104:3136]
	nonce := raw[3136:3168]
	return byteArraysToBytes([][]byte{s, hpk, nonce, t, rho})
}

func bindKyberSecret(unbound, ciphertext []byte) []byte {
	ctHash := sha3.Sum256(ciphertext)
	kdfInput := make([]byte, 0, len(unbound)+len(ctHash))
	kdfInput = append(kdfInput, unbound...)
	kdfInput = append(kdfInput, ctHash[:]...)
	out := make([]byte, len(unbound))
	shake := sha3.NewShake256()
	shake.Write(kdfInput)
	shake.Read(out)
	return out
}

func x25519ECDH(privateKey, publicKey []byte) ([]byte, error) {
	curve := ecdh.X25519()
	priv, err := curve.NewPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	pub, err := curve.NewPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	return priv.ECDH(pub)
}

func x25519PublicKey(privateKey []byte) []byte {
	priv, err := ecdh.X25519().NewPrivateKey(privateKey)
	if err != nil {
		panic(err)
	}
	return priv.PublicKey().Bytes()
}

type pqPublicKeys struct {
	x25519Pub []byte
	kyberPub  []byte
}

type pqKeyPairs struct {
	x25519Priv []byte
	kyberPriv  []byte
}

func derivePQKEK(senderIdentityPub, ephemeralPub []byte, recipient pqPublicKeys, kyberCiphertext, kyberSecret, ephSecret, authSecret []byte) ([]byte, error) {
	context := make([]byte, 0, len(senderIdentityPub)+len(ephemeralPub)+len(recipient.x25519Pub)+len(recipient.kyberPub)+len(kyberCiphertext)+1)
	context = append(context, senderIdentityPub...)
	context = append(context, ephemeralPub...)
	context = append(context, recipient.x25519Pub...)
	context = append(context, recipient.kyberPub...)
	context = append(context, kyberCiphertext...)
	context = append(context, cryptoProtocolTutaCrypt)
	ikm := make([]byte, 0, len(ephSecret)+len(authSecret)+len(kyberSecret))
	ikm = append(ikm, ephSecret...)
	ikm = append(ikm, authSecret...)
	ikm = append(ikm, kyberSecret...)
	return hkdf.Key(sha256.New, ikm, context, "kek", 32)
}

func pqDecapsulate(encoded []byte, recipient pqKeyPairs, recipientPub pqPublicKeys) (bucketKey, senderIdentityPub []byte, err error) {
	parts, err := bytesToByteArrays(encoded, 4)
	if err != nil {
		return nil, nil, err
	}
	senderIdentityPub, ephemeralPub, kyberCiphertext, kekEncBucketKey := parts[0], parts[1], parts[2], parts[3]
	ephSecret, err := x25519ECDH(recipient.x25519Priv, ephemeralPub)
	if err != nil {
		return nil, nil, err
	}
	authSecret, err := x25519ECDH(recipient.x25519Priv, senderIdentityPub)
	if err != nil {
		return nil, nil, err
	}
	kyberRaw, err := kyberPrivateKeyToRaw(recipient.kyberPriv)
	if err != nil {
		return nil, nil, err
	}
	var dk mlkem1024.PrivateKey
	if err := dk.Unpack(kyberRaw); err != nil {
		return nil, nil, err
	}
	kyberSecret := make([]byte, mlkem1024.SharedKeySize)
	dk.DecapsulateTo(kyberSecret, kyberCiphertext)
	kyberSecret = bindKyberSecret(kyberSecret, kyberCiphertext)
	kek, err := derivePQKEK(senderIdentityPub, ephemeralPub, recipientPub, kyberCiphertext, kyberSecret, ephSecret, authSecret)
	if err != nil {
		return nil, nil, err
	}
	bucketKey, err = decryptBytes(kek, kekEncBucketKey)
	if err != nil {
		return nil, nil, err
	}
	return bucketKey, senderIdentityPub, nil
}

func pqEncapsulate(senderIdentityPriv, ephemeralPriv []byte, recipient pqPublicKeys, bucketKey []byte) ([]byte, error) {
	ephSecret, err := x25519ECDH(ephemeralPriv, recipient.x25519Pub)
	if err != nil {
		return nil, err
	}
	authSecret, err := x25519ECDH(senderIdentityPriv, recipient.x25519Pub)
	if err != nil {
		return nil, err
	}
	kyberRaw, err := kyberPublicKeyToRaw(recipient.kyberPub)
	if err != nil {
		return nil, err
	}
	var ek mlkem1024.PublicKey
	if err := ek.Unpack(kyberRaw); err != nil {
		return nil, err
	}
	kyberCiphertext := make([]byte, mlkem1024.CiphertextSize)
	kyberSecret := make([]byte, mlkem1024.SharedKeySize)
	ek.EncapsulateTo(kyberCiphertext, kyberSecret, randomBytes(mlkem1024.EncapsulationSeedSize))
	kyberSecret = bindKyberSecret(kyberSecret, kyberCiphertext)
	kek, err := derivePQKEK(x25519PublicKey(senderIdentityPriv), x25519PublicKey(ephemeralPriv), recipient, kyberCiphertext, kyberSecret, ephSecret, authSecret)
	if err != nil {
		return nil, err
	}
	kekEncBucketKey := encryptBytes(kek, bucketKey)
	return byteArraysToBytes([][]byte{
		x25519PublicKey(senderIdentityPriv),
		x25519PublicKey(ephemeralPriv),
		kyberCiphertext,
		kekEncBucketKey,
	}), nil
}
