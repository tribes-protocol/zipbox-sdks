package zipbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
)

// A box envelope seals a credential the box itself consumes (a Tailscale auth
// key) to the box's own P-256 key, so only that box can open it. The scheme is
// the one the dashboard uses (packages/sandboxing BoxEnvelope.ts) and the box
// opens (apps/microvmd BoxEnvelope.ts): ephemeral P-256 ECDH against the box's
// agentPublicKey, HKDF-SHA256 with an empty salt and a purpose-bound info, then
// AES-256-GCM with the tag appended to the ciphertext. Every field is base64url
// without padding. sdks/conformance/vectors/box_envelope.json pins the bytes.

// boxEnvelope is the wire shape the control plane relays to the box unopened.
type boxEnvelope struct {
	EphemeralPublicKey string `json:"ephemeralPublicKey"`
	IV                 string `json:"iv"`
	Ciphertext         string `json:"ciphertext"`
}

const envelopeIVBytes = 12

// tailscaleEnvelopeInfo binds an envelope to one box and to the Tailscale
// purpose (foundation tailscaleBoxEnvelopeInfo).
func tailscaleEnvelopeInfo(sandboxID string) string {
	return "zipbox-tailscale-v1|" + sandboxID
}

// sealBoxEnvelope seals plaintext to the box key with fresh randomness.
func sealBoxEnvelope(agentPublicKey, info string, plaintext []byte) (*boxEnvelope, error) {
	ephemeral, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, envelopeIVBytes)
	cryptoRead(iv)
	return sealBoxEnvelopeWith(agentPublicKey, info, plaintext, ephemeral, iv)
}

// sealBoxEnvelopeWith is the deterministic core: the caller supplies the
// ephemeral key and iv. Only the conformance vector test passes fixed ones.
// Errors never carry the plaintext or the key.
func sealBoxEnvelopeWith(agentPublicKey, info string, plaintext []byte, ephemeral *ecdh.PrivateKey, iv []byte) (*boxEnvelope, error) {
	recipient, err := parseAgentPublicKey(agentPublicKey)
	if err != nil {
		return nil, err
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return nil, errors.New("zipbox: key agreement with the box key failed")
	}
	key, err := hkdf.Key(sha256.New, shared, nil, info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	spki, err := x509.MarshalPKIXPublicKey(ephemeral.PublicKey())
	if err != nil {
		return nil, err
	}
	return &boxEnvelope{
		EphemeralPublicKey: base64.RawURLEncoding.EncodeToString(spki),
		IV:                 base64.RawURLEncoding.EncodeToString(iv),
		Ciphertext:         base64.RawURLEncoding.EncodeToString(gcm.Seal(nil, iv, plaintext, nil)),
	}, nil
}

// parseAgentPublicKey reads the box key: SPKI DER in base64, standard or
// url-safe, padded or not.
func parseAgentPublicKey(value string) (*ecdh.PublicKey, error) {
	der, err := decodeBase64Any(value)
	if err != nil {
		return nil, errors.New("zipbox: the box's agentPublicKey is not base64")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("zipbox: the box's agentPublicKey is not an SPKI public key")
	}
	ec, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("zipbox: the box's agentPublicKey is not a P-256 key")
	}
	pub, err := ec.ECDH()
	if err != nil || pub.Curve() != ecdh.P256() {
		return nil, errors.New("zipbox: the box's agentPublicKey is not a P-256 key")
	}
	return pub, nil
}

func decodeBase64Any(value string) ([]byte, error) {
	normalized := strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimRight(strings.TrimSpace(value), "="))
	return base64.RawStdEncoding.DecodeString(normalized)
}
