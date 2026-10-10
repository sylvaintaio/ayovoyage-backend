package ticketsign

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSignVerify(t *testing.T) {
	s, _, err := New("k1", nil)
	if err != nil {
		t.Fatal(err)
	}
	in := Payload{TicketID: uuid.New(), DepartureID: uuid.New(), CompanyID: uuid.New(), Seat: "3A"}
	tok, err := s.Sign(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("vérification: %v", err)
	}
	if out.TicketID != in.TicketID || out.Seat != "3A" {
		t.Fatalf("payload inattendu: %+v", out)
	}

	// Toute modification du contenu invalide la signature.
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + parts[1][:len(parts[1])-2] + "AA." + parts[2]
	if _, err := s.Verify(forged); err == nil {
		t.Fatal("un billet falsifié a été accepté")
	}

	// Un billet signé par une autre clé est refusé.
	other, _, _ := New("k1", nil)
	tok2, _ := other.Sign(in)
	if _, err := s.Verify(tok2); err != ErrSignature {
		t.Fatalf("attendu ErrSignature, obtenu %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	oldSeedB64, oldPubB64, _ := GenerateKey()
	oldSeed, _ := base64.StdEncoding.DecodeString(oldSeedB64)
	oldSigner, _, _ := New("k1", oldSeed)
	ticket, err := oldSigner.Sign(Payload{TicketID: uuid.New(), Seat: "1A"})
	if err != nil {
		t.Fatal(err)
	}
	if PublicKeyOf(oldSeed) != oldPubB64 {
		t.Fatal("PublicKeyOf incohérent")
	}

	newSeedB64, _, _ := GenerateKey()
	newSeed, _ := base64.StdEncoding.DecodeString(newSeedB64)
	signer, _, _ := New("k2", newSeed)
	if _, err := signer.Verify(ticket); err != ErrUnknownKey {
		t.Fatalf("sans l'ancienne clé : %v, attendu ErrUnknownKey", err)
	}
	oldPub, _ := base64.StdEncoding.DecodeString(oldPubB64)
	if err := signer.AcceptKey("k1", oldPub); err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Verify(ticket); err != nil {
		t.Fatalf("ancien billet refusé après rotation : %v", err)
	}
	fresh, _ := signer.Sign(Payload{TicketID: uuid.New(), Seat: "2B"})
	if p, err := signer.Verify(fresh); err != nil || p.KeyID != "k2" {
		t.Fatalf("nouveau billet : %v (kid %s)", err, p.KeyID)
	}
	if len(signer.PublicKeys()) != 2 {
		t.Error("les terminaux doivent recevoir les deux clés publiques")
	}
	if err := signer.AcceptKey("k2", oldPub); err == nil {
		t.Error("un identifiant identique à la clé active doit être refusé")
	}
}
