// Package refgen génère des références lisibles et non devinables.
package refgen

import (
	"crypto/rand"
	"math/big"
)

// Alphabet sans caractères ambigus (0/O, 1/I/L).
const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func random(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

// Booking : référence de réservation, ex. BV7K3M9QXT.
func Booking() string { return "BV" + random(8) }

// Ticket : numéro de billet, ex. TK-4F8KQ2ZP9M.
func Ticket() string { return "TK-" + random(10) }

// Referral : code de parrainage client.
func Referral() string { return random(7) }

// Digits : code numérique (OTP).
func Digits(n int) string {
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			panic(err)
		}
		b[i] = byte('0' + v.Int64())
	}
	return string(b)
}

// Parcel : numéro de suivi d'un colis, ex. CL7K3M9QXT.
func Parcel() string { return "CL" + random(8) }

// Baggage : numéro d'étiquette bagage, ex. BG-4F8KQ2Z.
func Baggage() string { return "BG-" + random(7) }

// Incident : référence d'un dossier de réclamation, ex. RC-7K3M9Q.
func Incident() string { return "RC-" + random(6) }

// Promo : code promo à usage unique généré en série, ex. TABASKI-7K3M9Q.
func Promo(prefix string) string {
	if prefix == "" {
		return "PROMO-" + random(6)
	}
	return prefix + "-" + random(6)
}
