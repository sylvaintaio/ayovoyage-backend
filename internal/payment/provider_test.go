package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// Page de vérification Cloudflare renvoyée à la place de l'API : erreur reconnue, journal court.
func TestFedaPayCloudflareChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>Just a moment...</title></head></html>"))
	}))
	defer srv.Close()
	f := NewFedaPay("sk_sandbox_test", "", "sandbox")
	f.BaseURL = srv.URL
	_, err := f.Initiate(context.Background(), InitiateRequest{PaymentID: uuid.New(), Amount: 1000, Currency: "XOF", CustomerName: "Test"})
	if !errors.Is(err, ErrProviderBlocked) {
		t.Fatalf("erreur attendue ErrProviderBlocked, obtenu %v", err)
	}
	if len(err.Error()) > 200 {
		t.Fatalf("journal trop long : %q", err)
	}
}

// Erreur ordinaire de l'API (JSON) : message de FedaPay gardé pour le diagnostic.
func TestFedaPayAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Numéro invalide"}`))
	}))
	defer srv.Close()
	f := NewFedaPay("sk_sandbox_test", "", "sandbox")
	f.BaseURL = srv.URL
	_, err := f.Initiate(context.Background(), InitiateRequest{PaymentID: uuid.New(), Amount: 1000, Currency: "XOF", CustomerName: "Test"})
	if err == nil || errors.Is(err, ErrProviderBlocked) {
		t.Fatalf("erreur d'API attendue, obtenu %v", err)
	}
}

// Vente à bord sans e-mail : le champ email n'est pas envoyé (FedaPay refuse un e-mail vide).
func TestFedaPayCustomerWithoutEmail(t *testing.T) {
	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, body)
		if r.URL.Path == "/v1/transactions" {
			_, _ = w.Write([]byte(`{"v1/transaction":{"id":42,"status":"pending"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"t","url":"https://pay.test/t"}`))
	}))
	defer srv.Close()
	f := NewFedaPay("sk_sandbox_test", "", "sandbox")
	f.BaseURL = srv.URL
	for _, email := range []string{"", "  ", "pas-un-email"} {
		sent = nil
		res, err := f.Initiate(context.Background(), InitiateRequest{PaymentID: uuid.New(), Amount: 5000, Currency: "XOF",
			CustomerName: "Voyageur à bord", CustomerPhone: "+2290166000001", CustomerEmail: email})
		if err != nil || res.CheckoutURL != "https://pay.test/t" {
			t.Fatalf("e-mail %q : %v %+v", email, err, res)
		}
		customer := sent[0]["customer"].(map[string]any)
		if _, ok := customer["email"]; ok {
			t.Fatalf("e-mail %q envoyé alors qu'il est invalide : %v", email, customer)
		}
		if customer["phone_number"].(map[string]any)["number"] != "+2290166000001" {
			t.Fatalf("numéro absent : %v", customer)
		}
	}
	sent = nil
	_, _ = f.Initiate(context.Background(), InitiateRequest{PaymentID: uuid.New(), Amount: 5000, Currency: "XOF",
		CustomerName: "Afi Mensah", CustomerEmail: "afi@exemple.bj"})
	customer := sent[0]["customer"].(map[string]any)
	if customer["email"] != "afi@exemple.bj" || customer["phone_number"] != nil {
		t.Fatalf("e-mail valide gardé, numéro vide omis : %v", customer)
	}
}
