package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const issuer = "busvoyage"

type accessClaims struct {
	jwt.RegisteredClaims
	Type      SubjectType `json:"typ"`
	CompanyID string      `json:"cid,omitempty"`
	AgencyID  string      `json:"aid,omitempty"`
	Role      string      `json:"role,omitempty"`
	MFAAt     int64       `json:"mfa,omitempty"` // horodatage de la dernière vérification TOTP
}

type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
}

func NewTokenIssuer(secret []byte, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, ttl: ttl}
}

func (t *TokenIssuer) TTL() time.Duration { return t.ttl }

func (t *TokenIssuer) Issue(p Principal) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(t.ttl)
	c := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   p.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
		Type: p.Type,
		Role: p.Role,
	}
	if p.MFAAt != nil {
		c.MFAAt = p.MFAAt.Unix()
	}
	if p.IsStaff() {
		c.CompanyID = p.CompanyID.String()
		if p.AgencyID != nil {
			c.AgencyID = p.AgencyID.String()
		}
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(t.secret)
	return s, exp, err
}

func (t *TokenIssuer) Parse(token string) (Principal, error) {
	var c accessClaims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return Principal{}, err
	}
	p := Principal{Type: c.Type, Role: c.Role}
	if c.MFAAt > 0 {
		at := time.Unix(c.MFAAt, 0)
		p.MFAAt = &at
	}
	if p.ID, err = uuid.Parse(c.Subject); err != nil {
		return Principal{}, err
	}
	switch p.Type {
	case SubjectCustomer:
	case SubjectStaff:
		if p.CompanyID, err = uuid.Parse(c.CompanyID); err != nil {
			return Principal{}, err
		}
		if c.AgencyID != "" {
			aid, err := uuid.Parse(c.AgencyID)
			if err != nil {
				return Principal{}, err
			}
			p.AgencyID = &aid
		}
	default:
		return Principal{}, errors.New("type de sujet inconnu")
	}
	return p, nil
}

// NewOpaqueToken génère un jeton aléatoire (jeton de rafraîchissement, CSRF).
func NewOpaqueToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken : seul le hash des jetons de rafraîchissement est stocké.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- Jeton d'étape (double authentification)

const subjectMFAChallenge SubjectType = "mfa_challenge"

type challengeClaims struct {
	jwt.RegisteredClaims
	Type    SubjectType `json:"typ"`
	Purpose string      `json:"pur"` // verify ou setup
}

// IssueChallenge : jeton court (5 min) délivré après le mot de passe, qui ne
// permet que de vérifier ou d'activer la double authentification. Il est
// refusé partout ailleurs (Parse n'accepte que les types client/personnel).
func (t *TokenIssuer) IssueChallenge(staffID uuid.UUID, purpose string, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(ttl)
	c := challengeClaims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Subject: staffID.String(), Audience: jwt.ClaimStrings{"mfa"},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp), ID: uuid.NewString()},
		Type: subjectMFAChallenge, Purpose: purpose,
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(t.secret)
	return s, exp, err
}

func (t *TokenIssuer) ParseChallenge(token string) (uuid.UUID, string, error) {
	var c challengeClaims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer), jwt.WithAudience("mfa"), jwt.WithExpirationRequired(),
	)
	if err != nil || c.Type != subjectMFAChallenge {
		return uuid.Nil, "", errors.New("étape de connexion expirée")
	}
	id, err := uuid.Parse(c.Subject)
	return id, c.Purpose, err
}
