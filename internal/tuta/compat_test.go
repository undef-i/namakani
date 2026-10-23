package tuta

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudflare/circl/kem/mlkem/mlkem1024"
	"lukechampine.com/blake3"
)

type compatData struct {
	Aes128Tests []aesTest `json:"aes128Tests"`
	Aes256Tests []aesTest `json:"aes256Tests"`
	Aes128Mac   []aesTest `json:"aes128MacTests"`
	Bcrypt128   []kdfTest `json:"bcrypt128Tests"`
	Bcrypt256   []kdfTest `json:"bcrypt256Tests"`
	Argon2id    []kdfTest `json:"argon2idTests"`
	Hkdf        []struct {
		SaltHex     string `json:"saltHex"`
		IKMHex      string `json:"inputKeyMaterialHex"`
		InfoHex     string `json:"infoHex"`
		Length      int    `json:"lengthInBytes"`
		ExpectedHex string `json:"hkdfHex"`
	} `json:"hkdfTests"`
	Blake3 []struct {
		KeyHex       string `json:"keyHex"`
		Context      string `json:"context"`
		Length       int    `json:"lengthInBytes"`
		KdfOutputHex string `json:"kdfOutputHex"`
		DataHex      string `json:"dataHex"`
		TagHex       string `json:"tagHex"`
		DigestHex    string `json:"digestHex"`
	} `json:"blake3Tests"`
	Compression []struct {
		Uncompressed   string `json:"uncompressedText"`
		CompressedJava string `json:"compressedBase64TextJava"`
		CompressedJS   string `json:"compressedBase64TextJavaScript"`
		CompressedRust string `json:"compressedBase64TextRust"`
	} `json:"compressionTests"`
	ByteArrays []struct {
		Encoded string   `json:"encodedByteArrayAsHex"`
		Parts   []string `json:"byteArraysAsHex"`
	} `json:"byteArrayEncodingTests"`
	X25519 []struct {
		AlicePrivHex string `json:"alicePrivateKeyHex"`
		AlicePubHex  string `json:"alicePublicKeyHex"`
		EphPrivHex   string `json:"ephemeralPrivateKeyHex"`
		EphPubHex    string `json:"ephemeralPublicKeyHex"`
		BobPrivHex   string `json:"bobPrivateKeyHex"`
		BobPubHex    string `json:"bobPublicKeyHex"`
		EphSecretHex string `json:"ephemeralSharedSecretHex"`
		AuthSecretHx string `json:"authSharedSecretHex"`
	} `json:"x25519Tests"`
	Kyber []struct {
		PublicKey    string `json:"publicKey"`
		PrivateKey   string `json:"privateKey"`
		CipherText   string `json:"cipherText"`
		SharedSecret string `json:"sharedSecret"`
	} `json:"kyberEncryptionTests"`
	PQCrypt []struct {
		BucketKey     string `json:"bucketKey"`
		PublicX25519  string `json:"publicX25519Key"`
		PrivateX25519 string `json:"privateX25519Key"`
		PublicKyber   string `json:"publicKyberKey"`
		PrivateKyber  string `json:"privateKyberKey"`
		PQMessage     string `json:"pqMessage"`
	} `json:"pqcryptEncryptionTests"`
	Aead []struct {
		PlaintextB64   string `json:"plaintextBase64"`
		CiphertextB64  string `json:"ciphertextBase64"`
		PlaintextKey   string `json:"plaintextKey"`
		EncryptionKey  string `json:"encryptionKey"`
		AuthKey        string `json:"authenticationKey"`
		EncryptedKey   string `json:"encryptedKey"`
		Seed           string `json:"seed"`
		AssociatedData string `json:"associatedData"`
	} `json:"aeadTests"`
	AeadKeyDerivation []struct {
		GroupKey256Hex string `json:"groupKey256Hex"`
		GroupKey128Hex string `json:"groupKey128Hex"`
		SessionKeyHex  string `json:"sessionKeyHex"`
		KdfNonceHex    string `json:"kdfNonceHex"`
		GlobalTypeID   string `json:"globalInstanceTypeId"`
		EncFrom256     string `json:"encryptionKeyFrom256Hex"`
		AuthFrom256    string `json:"authenticationKeyFrom256Hex"`
		EncFrom128     string `json:"encryptionKeyFrom128Hex"`
		AuthFrom128    string `json:"authenticationKeyFrom128Hex"`
		EncFromSession string `json:"encryptionKeyFromSessionKeyHex"`
		AuthFromSess   string `json:"authenticationKeyFromSessionKeyHex"`
	} `json:"aeadKeyDerivationTests"`
}

type aesTest struct {
	PlaintextB64    string  `json:"plainTextBase64"`
	CiphertextB64   string  `json:"cipherTextBase64"`
	KeyHex          string  `json:"hexKey"`
	KeyToEncrypt256 *string `json:"keyToEncrypt256"`
	KeyToEncrypt128 *string `json:"keyToEncrypt128"`
	EncryptedKey256 *string `json:"encryptedKey256"`
	EncryptedKey128 *string `json:"encryptedKey128"`
}

type kdfTest struct {
	Password string `json:"password"`
	KeyHex   string `json:"keyHex"`
	SaltHex  string `json:"saltHex"`
}

func loadCompat(t *testing.T) compatData {
	t.Helper()
	raw, err := os.ReadFile("testdata/CompatibilityTestData.json")
	if err != nil {
		t.Fatal(err)
	}
	var d compatData
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runAesTests(t *testing.T, tests []aesTest) {
	for i, td := range tests {
		key := unhex(t, td.KeyHex)
		plain := unb64(t, td.PlaintextB64)
		ct := unb64(t, td.CiphertextB64)
		got, err := decryptBytes(key, ct)
		if err != nil {
			t.Fatalf("case %d decrypt: %v", i, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("case %d decrypt mismatch", i)
		}
		if td.EncryptedKey128 != nil {
			dec, err := decryptKey(key, unb64(t, *td.EncryptedKey128))
			if err != nil {
				t.Fatalf("case %d decryptKey128: %v", i, err)
			}
			if hex.EncodeToString(dec) != *td.KeyToEncrypt128 {
				t.Fatalf("case %d decryptKey128 mismatch", i)
			}
		}
		if td.EncryptedKey256 != nil {
			dec, err := decryptKey(key, unb64(t, *td.EncryptedKey256))
			if err != nil {
				t.Fatalf("case %d decryptKey256: %v", i, err)
			}
			if hex.EncodeToString(dec) != *td.KeyToEncrypt256 {
				t.Fatalf("case %d decryptKey256 mismatch", i)
			}
		}
	}
}

func TestCompatAes128(t *testing.T) { runAesTests(t, loadCompat(t).Aes128Tests) }
func TestCompatAes256(t *testing.T) { runAesTests(t, loadCompat(t).Aes256Tests) }
func TestCompatAes128Mac(t *testing.T) {
	runAesTests(t, loadCompat(t).Aes128Mac)
}

func TestCompatBcrypt(t *testing.T) {
	d := loadCompat(t)
	for i, td := range d.Bcrypt128 {
		key, err := passphraseKeyBcrypt(td.Password, unhex(t, td.SaltHex), 128)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(key) != td.KeyHex {
			t.Fatalf("bcrypt128 case %d mismatch: got %x want %s", i, key, td.KeyHex)
		}
	}
	for i, td := range d.Bcrypt256 {
		key, err := passphraseKeyBcrypt(td.Password, unhex(t, td.SaltHex), 256)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(key) != td.KeyHex {
			t.Fatalf("bcrypt256 case %d mismatch", i)
		}
	}
}

func TestCompatArgon2id(t *testing.T) {
	for i, td := range loadCompat(t).Argon2id {
		key := passphraseKeyArgon2id(td.Password, unhex(t, td.SaltHex))
		if hex.EncodeToString(key) != td.KeyHex {
			t.Fatalf("argon2id case %d mismatch", i)
		}
	}
}

func TestCompatHkdf(t *testing.T) {
	for i, td := range loadCompat(t).Hkdf {
		out, err := hkdf.Key(sha256.New, unhex(t, td.IKMHex), unhex(t, td.SaltHex), string(unhex(t, td.InfoHex)), td.Length)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(out) != td.ExpectedHex {
			t.Fatalf("hkdf case %d mismatch", i)
		}
	}
}

func TestCompatBlake3(t *testing.T) {
	for i, td := range loadCompat(t).Blake3 {
		data := unhex(t, td.DataHex)
		h := blake3.New(32, nil)
		h.Write(data)
		if hex.EncodeToString(h.Sum(nil)) != td.DigestHex {
			t.Fatalf("blake3 hash case %d mismatch", i)
		}
		tag := blake3MAC(unhex(t, td.KeyHex), data)
		if hex.EncodeToString(tag) != td.TagHex {
			t.Fatalf("blake3 mac case %d mismatch", i)
		}
		kdf := blake3KDF(unhex(t, td.KeyHex), td.Context, td.Length)
		if hex.EncodeToString(kdf) != td.KdfOutputHex {
			t.Fatalf("blake3 kdf case %d mismatch", i)
		}
	}
}

func TestCompatCompression(t *testing.T) {
	for i, td := range loadCompat(t).Compression {
		for name, enc := range map[string]string{"java": td.CompressedJava, "js": td.CompressedJS, "rust": td.CompressedRust} {
			if enc == "" {
				continue
			}
			out, err := lz4Decompress(unb64(t, enc))
			if err != nil {
				t.Fatalf("case %d %s: %v", i, name, err)
			}
			if string(out) != td.Uncompressed {
				t.Fatalf("case %d %s mismatch", i, name)
			}
		}
		rt, err := lz4Decompress(lz4Compress([]byte(td.Uncompressed)))
		if err != nil {
			t.Fatal(err)
		}
		if string(rt) != td.Uncompressed {
			t.Fatalf("case %d round trip mismatch", i)
		}
	}
}

func TestCompatByteArrays(t *testing.T) {
	for i, td := range loadCompat(t).ByteArrays {
		if td.Encoded == "" {
			continue
		}
		parts := make([][]byte, len(td.Parts))
		for j, p := range td.Parts {
			parts[j] = unhex(t, p)
		}
		if hex.EncodeToString(byteArraysToBytes(parts)) != td.Encoded {
			t.Fatalf("case %d encode mismatch", i)
		}
		back, err := bytesToByteArrays(unhex(t, td.Encoded), len(parts))
		if err != nil {
			t.Fatal(err)
		}
		for j := range parts {
			if !bytes.Equal(back[j], parts[j]) {
				t.Fatalf("case %d decode mismatch", i)
			}
		}
	}
}

func TestCompatX25519(t *testing.T) {
	for _, td := range loadCompat(t).X25519 {
		eph, err := x25519ECDH(unhex(t, td.EphPrivHex), unhex(t, td.BobPubHex))
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(eph) != td.EphSecretHex {
			t.Fatal("ephemeral shared secret mismatch")
		}
		auth, err := x25519ECDH(unhex(t, td.AlicePrivHex), unhex(t, td.BobPubHex))
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(auth) != td.AuthSecretHx {
			t.Fatal("auth shared secret mismatch")
		}
		ephBack, err := x25519ECDH(unhex(t, td.BobPrivHex), unhex(t, td.EphPubHex))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ephBack, eph) {
			t.Fatal("decapsulate side mismatch")
		}
	}
}

func TestCompatKyber(t *testing.T) {
	for i, td := range loadCompat(t).Kyber {
		pubEnc := unhex(t, td.PublicKey)
		rawPub, err := kyberPublicKeyToRaw(pubEnc)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(kyberPublicKeyFromRaw(rawPub), pubEnc) {
			t.Fatalf("case %d public key round trip mismatch", i)
		}
		privEnc := unhex(t, td.PrivateKey)
		rawPriv, err := kyberPrivateKeyToRaw(privEnc)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(kyberPrivateKeyFromRaw(rawPriv), privEnc) {
			t.Fatalf("case %d private key round trip mismatch", i)
		}
		var dk mlkem1024.PrivateKey
		if err := dk.Unpack(rawPriv); err != nil {
			t.Fatal(err)
		}
		ss := make([]byte, mlkem1024.SharedKeySize)
		dk.DecapsulateTo(ss, unhex(t, td.CipherText))
		ss = bindKyberSecret(ss, unhex(t, td.CipherText))
		if hex.EncodeToString(ss) != td.SharedSecret {
			t.Fatalf("case %d decapsulate mismatch", i)
		}
		packed := make([]byte, mlkem1024.PrivateKeySize)
		dk.Pack(packed)
		if !bytes.Equal(packed, rawPriv) {
			t.Fatalf("case %d private key pack mismatch", i)
		}
	}
}

func TestCompatPQCrypt(t *testing.T) {
	for i, td := range loadCompat(t).PQCrypt {
		pairs := pqKeyPairs{x25519Priv: unhex(t, td.PrivateX25519), kyberPriv: unhex(t, td.PrivateKyber)}
		pubs := pqPublicKeys{x25519Pub: unhex(t, td.PublicX25519), kyberPub: unhex(t, td.PublicKyber)}
		bucketKey, senderPub, err := pqDecapsulate(unhex(t, td.PQMessage), pairs, pubs)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if hex.EncodeToString(bucketKey) != td.BucketKey {
			t.Fatalf("case %d bucket key mismatch", i)
		}
		if !bytes.Equal(senderPub, pubs.x25519Pub) {
			t.Fatalf("case %d sender key mismatch", i)
		}
	}
}

func TestCompatAead(t *testing.T) {
	for i, td := range loadCompat(t).Aead {
		sub := aeadSubKeys{encKey: unhex(t, td.EncryptionKey), macKey: unhex(t, td.AuthKey)}
		ad := unb64(t, td.AssociatedData)
		seed := unhex(t, td.Seed)
		iv := seed[:16]
		plain := unb64(t, td.PlaintextB64)
		ct, tag := aeadEncryptWithIV(sub, iv, plain, ad)
		versioned := append([]byte{3}, iv...)
		versioned = append(versioned, ct...)
		versioned = append(versioned, tag...)
		if base64.StdEncoding.EncodeToString(versioned) != td.CiphertextB64 {
			t.Fatalf("case %d encrypt mismatch", i)
		}
		parsed, err := parseCiphertext(versioned)
		if err != nil {
			t.Fatal(err)
		}
		back, err := aeadDecrypt(sub, parsed, ad)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(back, plain) {
			t.Fatalf("case %d decrypt mismatch", i)
		}
		keyPlain := unhex(t, td.PlaintextKey)
		kct, ktag := aeadEncryptWithIV(sub, iv, keyPlain, ad)
		kVersioned := append([]byte{3}, iv...)
		kVersioned = append(kVersioned, kct...)
		kVersioned = append(kVersioned, ktag...)
		if base64.StdEncoding.EncodeToString(kVersioned) != td.EncryptedKey {
			t.Fatalf("case %d key encrypt mismatch", i)
		}
	}
}

func TestCompatAeadKeyDerivation(t *testing.T) {
	for i, td := range loadCompat(t).AeadKeyDerivation {
		parts := strings.Split(td.GlobalTypeID, "/")
		idNum, err := strconv.Atoi(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		instType := typeID{app: parts[0], id: idNum}
		nonce := unhex(t, td.KdfNonceHex)
		from256 := deriveSubKeysFromGroupKey(unhex(t, td.GroupKey256Hex), nonce, instType)
		if hex.EncodeToString(from256.encKey) != td.EncFrom256 || hex.EncodeToString(from256.macKey) != td.AuthFrom256 {
			t.Fatalf("case %d group256 mismatch", i)
		}
		from128 := deriveSubKeysFromGroupKey(unhex(t, td.GroupKey128Hex), nonce, instType)
		if hex.EncodeToString(from128.encKey) != td.EncFrom128 || hex.EncodeToString(from128.macKey) != td.AuthFrom128 {
			t.Fatalf("case %d group128 mismatch", i)
		}
		fromSession := deriveSubKeysFromSessionKey(unhex(t, td.SessionKeyHex), instType)
		if hex.EncodeToString(fromSession.encKey) != td.EncFromSession || hex.EncodeToString(fromSession.macKey) != td.AuthFromSess {
			t.Fatalf("case %d session mismatch", i)
		}
	}
}
