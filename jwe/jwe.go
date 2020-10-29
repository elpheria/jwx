//go:generate go run internal/cmd/genheader/main.go

// Package jwe implements JWE as described in https://tools.ietf.org/html/rfc7516
package jwe

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"

	"github.com/lestrrat-go/jwx/internal/json"

	"github.com/lestrrat-go/jwx/buffer"
	"github.com/lestrrat-go/jwx/jwa"
	"github.com/lestrrat-go/jwx/jwe/internal/content_crypt"
	"github.com/lestrrat-go/jwx/jwe/internal/keyenc"
	"github.com/lestrrat-go/jwx/jwe/internal/keygen"
	"github.com/lestrrat-go/pdebug"
	"github.com/pkg/errors"
)

// Encrypt takes the plaintext payload and encrypts it in JWE compact format.
func Encrypt(payload []byte, keyalg jwa.KeyEncryptionAlgorithm, key interface{}, contentalg jwa.ContentEncryptionAlgorithm, compressalg jwa.CompressionAlgorithm) ([]byte, error) {
	contentcrypt, err := content_crypt.NewAES(contentalg)
	if err != nil {
		return nil, errors.Wrap(err, `failed to create AES encrypter`)
	}

	var enc keyenc.Encrypter
	var keysize int
	switch keyalg {
	case jwa.RSA1_5:
		var pubkey *rsa.PublicKey
		switch v := key.(type) {
		case rsa.PublicKey:
			pubkey = &v
		case *rsa.PublicKey:
			pubkey = v
		default:
			return nil, errors.Errorf("*rsa.PublicKey is required as the key to build %s key encrypter", keyalg)
		}

		enc, err = keyenc.NewRSAPKCSEncrypt(keyalg, pubkey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create RSA PKCS encrypter")
		}
		keysize = contentcrypt.KeySize() / 2
	case jwa.RSA_OAEP, jwa.RSA_OAEP_256:
		var pubkey *rsa.PublicKey
		switch v := key.(type) {
		case rsa.PublicKey:
			pubkey = &v
		case *rsa.PublicKey:
			pubkey = v
		default:
			return nil, errors.Errorf("*rsa.PublicKey is required as the key to build %s key encrypter", keyalg)
		}

		enc, err = keyenc.NewRSAOAEPEncrypt(keyalg, pubkey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create RSA OAEP encrypter")
		}
		keysize = contentcrypt.KeySize() / 2
	case jwa.A128KW, jwa.A192KW, jwa.A256KW:
		sharedkey, ok := key.([]byte)
		if !ok {
			return nil, errors.New("invalid key: []byte required")
		}
		enc, err = keyenc.NewAESCGM(keyalg, sharedkey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create key wrap encrypter")
		}
		keysize = contentcrypt.KeySize()
		switch aesKeySize := keysize / 2; aesKeySize {
		case 16, 24, 32:
		default:
			return nil, errors.Errorf("unsupported keysize %d (from content encryption algorithm %s). consider using content encryption that uses 32, 48, or 64 byte keys", keysize, contentalg)
		}
	case jwa.ECDH_ES, jwa.ECDH_ES_A128KW, jwa.ECDH_ES_A192KW, jwa.ECDH_ES_A256KW:
		var pubkey *ecdsa.PublicKey
		switch v := key.(type) {
		case ecdsa.PublicKey:
			pubkey = &v
		case *ecdsa.PublicKey:
			pubkey = v
		default:
			return nil, errors.Errorf("invalid key: *ecdsa.PublicKey required (got %T)", key)
		}

		enc, err = keyenc.NewECDHESEncrypt(keyalg, pubkey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create ECDHS key wrap encrypter")
		}
		keysize = contentcrypt.KeySize() / 2
//	case jwa.ECDH_ES:
//		enc, err = keyenc.NewECDHESEncrypt(keyalg, pubkey)
	case jwa.A128GCMKW, jwa.A192GCMKW, jwa.A256GCMKW:
		fallthrough
	case jwa.PBES2_HS256_A128KW, jwa.PBES2_HS384_A192KW, jwa.PBES2_HS512_A256KW:
		fallthrough
	default:
		if pdebug.Enabled {
			pdebug.Printf("Encrypt: unknown key encryption algorithm: %s", keyalg)
		}
		return nil, errors.Errorf(`invalid key encryption algorithm (%s)`, keyalg)
	}

	if pdebug.Enabled {
		pdebug.Printf("Encrypt: keysize = %d", keysize)
	}
	encctx := getEncryptCtx()
	defer releaseEncryptCtx(encctx)

	encctx.contentEncrypter = contentcrypt
	encctx.generator = keygen.NewRandom(keysize)
	encctx.keyEncrypters = []keyenc.Encrypter{enc}
	encctx.compress = compressalg
	msg, err := encctx.Encrypt(payload)
	if err != nil {
		if pdebug.Enabled {
			pdebug.Printf("Encrypt: failed to encrypt: %s", err)
		}
		return nil, errors.Wrap(err, "failed to encrypt payload")
	}

	return Compact(msg)
}

// Decrypt takes the key encryption algorithm and the corresponding
// key to decrypt the JWE message, and returns the decrypted payload.
// The JWE message can be either compact or full JSON format.
func Decrypt(buf []byte, alg jwa.KeyEncryptionAlgorithm, key interface{}) ([]byte, error) {
	msg, err := Parse(buf)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse buffer for Decrypt")
	}

	return msg.Decrypt(alg, key)
}

// Parse parses the JWE message into a Message object. The JWE message
// can be either compact or full JSON format.
func Parse(buf []byte) (*Message, error) {
	buf = bytes.TrimSpace(buf)
	if len(buf) == 0 {
		return nil, errors.New("empty buffer")
	}

	if buf[0] == '{' {
		return parseJSON(buf)
	}
	return parseCompact(buf)
}

// ParseString is the same as Parse, but takes a string.
func ParseString(s string) (*Message, error) {
	return Parse([]byte(s))
}

func parseJSON(buf []byte) (*Message, error) {
	m := NewMessage()
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, errors.Wrap(err, "failed to parse JSON")
	}
	return m, nil
}

func parseCompact(buf []byte) (*Message, error) {
	if pdebug.Enabled {
		pdebug.Printf("Parse(Compact): buf = '%s'", buf)
	}
	parts := bytes.Split(buf, []byte{'.'})
	if len(parts) != 5 {
		return nil, errors.Errorf(`compact JWE format must have five parts (%d)`, len(parts))
	}

	hdrbuf := buffer.Buffer{}
	if err := hdrbuf.Base64Decode(parts[0]); err != nil {
		return nil, errors.Wrap(err, `failed to parse first part of compact form`)
	}
	if pdebug.Enabled {
		pdebug.Printf("hdrbuf = %s", hdrbuf)
	}

	protected := NewHeaders()
	if err := json.Unmarshal(hdrbuf, protected); err != nil {
		return nil, errors.Wrap(err, "failed to parse header JSON")
	}

	var enckeybuf buffer.Buffer
	if err := enckeybuf.Base64Decode(parts[1]); err != nil {
		return nil, errors.Wrap(err, "failed to base64 decode encryption key")
	}

	var ivbuf buffer.Buffer
	if err := ivbuf.Base64Decode(parts[2]); err != nil {
		return nil, errors.Wrap(err, "failed to base64 decode iv")
	}

	var ctbuf buffer.Buffer
	if err := ctbuf.Base64Decode(parts[3]); err != nil {
		return nil, errors.Wrap(err, "failed to base64 decode content")
	}

	var tagbuf buffer.Buffer
	if err := tagbuf.Base64Decode(parts[4]); err != nil {
		return nil, errors.Wrap(err, "failed to base64 decode tag")
	}

	m := NewMessage()
	if err := m.Set(CipherTextKey, ctbuf); err != nil {
		return nil, errors.Wrapf(err, `failed to set %s`, CipherTextKey)
	}
	if err := m.Set(InitializationVectorKey, ivbuf); err != nil {
		return nil, errors.Wrapf(err, `failed to set %s`, InitializationVectorKey)
	}
	if err := m.Set(ProtectedHeadersKey, protected); err != nil {
		return nil, errors.Wrapf(err, `failed to set %s`, ProtectedHeadersKey)
	}

	// Recipients in this case should not contain the content encryption key,
	// so move that out
	hdrs, err := mergeHeaders(context.TODO(), nil, protected)
	if err != nil {
		return nil, errors.Wrap(err, `failed to clone headers`)
	}

	if err := hdrs.Remove(ContentEncryptionKey); err != nil {
		return nil, errors.Wrapf(err, "failed to remove %#v from public header", ContentEncryptionKey)
	}
	if err := m.Set(RecipientsKey, []Recipient{
		&stdRecipient{
			headers:      hdrs,
			encryptedKey: enckeybuf,
		},
	}); err != nil {
		return nil, errors.Wrapf(err, `failed to set %s`, RecipientsKey)
	}
	if err := m.Set(TagKey, tagbuf); err != nil {
		return nil, errors.Wrapf(err, `failed to set %s`, TagKey)
	}
	return m, nil
}
