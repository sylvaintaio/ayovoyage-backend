package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Double authentification par application (TOTP, RFC 6238) : codes à 6
// chiffres renouvelés toutes les 30 secondes, compatibles Google
// Authenticator, Microsoft Authenticator, 2FAS, Authy…

const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1 // ±30 s de décalage d'horloge toléré
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret génère un secret de 160 bits encodé en base32 (format attendu par les applications).
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// TOTPURI construit le lien otpauth:// encodé dans le QR code d'activation.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprint(totpDigits))
	v.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + v.Encode()
}

func totpCode(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, bin%1_000_000)
}

// ValidateTOTP vérifie un code et renvoie le pas de temps utilisé. Un code dont
// le pas est inférieur ou égal à lastStep est refusé (pas de rejeu).
func ValidateTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	for d := -totpSkew; d <= totpSkew; d++ {
		step := cur + int64(d)
		if step <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(key, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------- Codes de secours

const recoveryAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // sans 0/O, 1/I

// NewRecoveryCodes génère n codes de secours au format XXXX-XXXX.
func NewRecoveryCodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		var s strings.Builder
		for j, c := range b {
			if j == 4 {
				s.WriteByte('-')
			}
			s.WriteByte(recoveryAlphabet[int(c)%len(recoveryAlphabet)])
		}
		out[i] = s.String()
	}
	return out
}

// NormalizeRecoveryCode : insensible à la casse, aux espaces et au tiret.
func NormalizeRecoveryCode(code string) string {
	r := strings.NewReplacer("-", "", " ", "")
	return strings.ToUpper(r.Replace(strings.TrimSpace(code)))
}

// HashRecoveryCode : seul le hash est conservé.
func HashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte("bv-recovery:" + NormalizeRecoveryCode(code)))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- Chiffrement des secrets

// SecretBox chiffre les secrets TOTP en base (AES-256-GCM) : une copie de la
// base seule ne permet pas de générer des codes.
type SecretBox struct{ aead cipher.AEAD }

func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, errors.New("la clé de chiffrement doit faire 32 octets")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

func (b *SecretBox) Seal(plain string) string {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.RawStdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil))
}

func (b *SecretBox) Open(sealed string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("secret chiffré illisible")
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("secret chiffré illisible (clé MFA_ENCRYPTION_KEY modifiée ?)")
	}
	return string(plain), nil
}
