// Package phone normalise les numéros au format E.164.
package phone

import (
	"errors"
	"regexp"
	"strings"
)

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

var ErrInvalid = errors.New("numéro de téléphone invalide")

// Normalize convertit un numéro saisi librement en E.164.
// Sans indicatif, defaultCC (ex. "229" pour le Bénin) est ajouté.
func Normalize(raw, defaultCC string) (string, error) {
	s := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == '+' {
			return r
		}
		return -1
	}, raw)
	switch {
	case strings.HasPrefix(s, "+"):
	case strings.HasPrefix(s, "00"):
		s = "+" + s[2:]
	case strings.HasPrefix(s, defaultCC) && len(s) > len(defaultCC)+7:
		s = "+" + s
	default:
		s = "+" + defaultCC + s
	}
	if !e164.MatchString(s) {
		return "", ErrInvalid
	}
	return s, nil
}

// Mask masque un numéro pour l'affichage public : +22901****5678.
func Mask(p string) string {
	if len(p) < 8 {
		return "****"
	}
	return p[:len(p)-8] + "****" + p[len(p)-4:]
}
