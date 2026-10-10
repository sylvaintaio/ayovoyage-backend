// Package ticketsign signe et vérifie le contenu des QR codes des billets.
//
// Format : BV1.<payload base64url>.<signature Ed25519 base64url>
// La clé privée ne quitte jamais le serveur ; l'application Agent reçoit
// uniquement la clé publique et peut donc vérifier un billet hors connexion.
package ticketsign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const prefix = "BV1"

var (
	ErrMalformed  = errors.New("QR code illisible")
	ErrSignature  = errors.New("signature du billet invalide")
	ErrUnknownKey = errors.New("clé de signature inconnue")
)

type Payload struct {
	TicketID    uuid.UUID `json:"tid"`
	DepartureID uuid.UUID `json:"dep"`
	CompanyID   uuid.UUID `json:"cid"`
	Seat        string    `json:"seat"`
	IssuedAt    int64     `json:"iat"`
	KeyID       string    `json:"kid"`
}

type PublicKey struct {
	KeyID     string `json:"kid" doc:"Identifiant de la clé"`
	Algorithm string `json:"alg" doc:"Algorithme (Ed25519)"`
	PublicKey string `json:"public_key" doc:"Clé publique encodée en base64"`
}

type Signer struct {
	keyID string
	priv  ed25519.PrivateKey
	// Clés publiques acceptées (rotation : l'ancienne reste valide le temps nécessaire).
	pubs map[string]ed25519.PublicKey
}

// New crée un signataire. Si seed est vide (développement uniquement),
// une clé éphémère est générée : les billets deviennent invalides au redémarrage.
func New(keyID string, seed []byte) (*Signer, bool, error) {
	ephemeral := false
	if len(seed) == 0 {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, false, err
		}
		ephemeral = true
	}
	if len(seed) != ed25519.SeedSize {
		return nil, false, errors.New("seed Ed25519 invalide")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{
		keyID: keyID,
		priv:  priv,
		pubs:  map[string]ed25519.PublicKey{keyID: priv.Public().(ed25519.PublicKey)},
	}, ephemeral, nil
}

// AcceptKey ajoute une ancienne clé publique encore acceptée à la vérification
// (rotation : les billets déjà émis restent valables jusqu'à leur voyage).
func (s *Signer) AcceptKey(keyID string, pub []byte) error {
	if keyID == s.keyID {
		return errors.New("l'identifiant de l'ancienne clé doit différer de celui de la clé active")
	}
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("clé publique Ed25519 invalide")
	}
	s.pubs[keyID] = ed25519.PublicKey(pub)
	return nil
}

// KeyID : identifiant de la clé qui signe les nouveaux billets.
func (s *Signer) KeyID() string { return s.keyID }

// GenerateKey produit une nouvelle seed (privée) et la clé publique associée, en base64.
func GenerateKey() (seedB64, pubB64 string, err error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", "", err
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(seed), base64.StdEncoding.EncodeToString(pub), nil
}

// PublicKeyOf renvoie la clé publique (base64) correspondant à une seed.
func PublicKeyOf(seed []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
}

func (s *Signer) Sign(p Payload) (string, error) {
	p.KeyID = s.keyID
	if p.IssuedAt == 0 {
		p.IssuedAt = time.Now().Unix()
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	signed := prefix + "." + base64.RawURLEncoding.EncodeToString(raw)
	sig := ed25519.Sign(s.priv, []byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *Signer) Verify(token string) (Payload, error) {
	var p Payload
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != prefix {
		return p, ErrMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return p, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return p, ErrMalformed
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, ErrMalformed
	}
	pub, ok := s.pubs[p.KeyID]
	if !ok {
		return p, ErrUnknownKey
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return p, ErrSignature
	}
	return p, nil
}

func (s *Signer) PublicKeys() []PublicKey {
	out := make([]PublicKey, 0, len(s.pubs))
	for kid, pub := range s.pubs {
		out = append(out, PublicKey{KeyID: kid, Algorithm: "Ed25519", PublicKey: base64.StdEncoding.EncodeToString(pub)})
	}
	return out
}
