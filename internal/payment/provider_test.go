package payment

import (
	"context"
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
