// Package payment gère l'encaissement (Mobile Money / carte via agrégateur),
// les notifications des fournisseurs et les remboursements.
package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type InitiateRequest struct {
	PaymentID     uuid.UUID
	Amount        int64
	Currency      string
	Description   string
	CustomerName  string
	CustomerPhone string
	CustomerEmail string
	CallbackURL   string // retour du client après paiement
}

type InitiateResult struct {
	ProviderReference string
	CheckoutURL       string
}

// State normalisé d'une transaction chez le fournisseur.
type State string

const (
	StatePending   State = "pending"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

type Status struct {
	Reference string
	State     State
	Amount    int64
	Currency  string
}

// Provider : interface commune aux agrégateurs (FedaPay, KKiaPay, CinetPay…).
type Provider interface {
	Name() string
	Initiate(ctx context.Context, req InitiateRequest) (InitiateResult, error)
	// FetchStatus interroge le fournisseur : c'est la source de vérité.
	FetchStatus(ctx context.Context, reference string) (Status, error)
	// VerifyWebhook vérifie la signature et renvoie (identifiant d'événement, référence de transaction).
	VerifyWebhook(signature string, body []byte) (eventID, reference string, err error)
}

var ErrBadSignature = errors.New("signature de webhook invalide")

// ---------------------------------------------------------------- Mock (développement)

// Mock simule un fournisseur : le paiement est validé via POST /v1/dev/payments/{id}/simulate.
type Mock struct {
	APIURL string
}

func (Mock) Name() string { return "mock" }

func (m Mock) Initiate(_ context.Context, req InitiateRequest) (InitiateResult, error) {
	return InitiateResult{
		ProviderReference: "mock_" + req.PaymentID.String(),
		CheckoutURL:       m.APIURL + "/v1/dev/payments/" + req.PaymentID.String() + "/simulate",
	}, nil
}

func (Mock) FetchStatus(_ context.Context, reference string) (Status, error) {
	return Status{Reference: reference, State: StatePending}, nil
}

func (Mock) VerifyWebhook(string, []byte) (string, string, error) {
	return "", "", errors.New("webhooks non supportés par le fournisseur mock")
}

// ---------------------------------------------------------------- FedaPay
//
// Intégration à valider en sandbox avec la documentation officielle FedaPay
// (https://docs.fedapay.com) avant la mise en production. Par sécurité, le
// statut annoncé par un webhook n'est jamais cru tel quel : il est relu via l'API.

type FedaPay struct {
	SecretKey     string
	WebhookSecret string
	BaseURL       string
	HTTP          *http.Client
}

func NewFedaPay(secretKey, webhookSecret, env string) *FedaPay {
	base := "https://sandbox-api.fedapay.com"
	if env == "live" {
		base = "https://api.fedapay.com"
	}
	return &FedaPay{SecretKey: secretKey, WebhookSecret: webhookSecret, BaseURL: base, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (*FedaPay) Name() string { return "fedapay" }

func (f *FedaPay) call(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.BaseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("fedapay %s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(raw), 300))
	}
	return json.Unmarshal(raw, out)
}

type fedaTransaction struct {
	ID       int64  `json:"id"`
	Status   string `json:"status"`
	Amount   int64  `json:"amount"`
	Currency struct {
		ISO string `json:"iso"`
	} `json:"currency"`
}

func (f *FedaPay) Initiate(ctx context.Context, req InitiateRequest) (InitiateResult, error) {
	first, last := splitName(req.CustomerName)
	payload := map[string]any{
		"description":  req.Description,
		"amount":       req.Amount,
		"currency":     map[string]string{"iso": req.Currency},
		"callback_url": req.CallbackURL,
		"customer": map[string]any{
			"firstname": first, "lastname": last, "email": req.CustomerEmail,
			"phone_number": map[string]string{"number": req.CustomerPhone, "country": "bj"},
		},
		"custom_metadata": map[string]string{"payment_id": req.PaymentID.String()},
	}
	var created struct {
		Transaction fedaTransaction `json:"v1/transaction"`
	}
	if err := f.call(ctx, http.MethodPost, "/v1/transactions", payload, &created); err != nil {
		return InitiateResult{}, err
	}
	var token struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	ref := strconv.FormatInt(created.Transaction.ID, 10)
	if err := f.call(ctx, http.MethodPost, "/v1/transactions/"+ref+"/token", nil, &token); err != nil {
		return InitiateResult{}, err
	}
	return InitiateResult{ProviderReference: ref, CheckoutURL: token.URL}, nil
}

func (f *FedaPay) FetchStatus(ctx context.Context, reference string) (Status, error) {
	if _, err := strconv.ParseInt(reference, 10, 64); err != nil {
		return Status{}, errors.New("référence FedaPay invalide")
	}
	var res struct {
		Transaction fedaTransaction `json:"v1/transaction"`
	}
	if err := f.call(ctx, http.MethodGet, "/v1/transactions/"+reference, nil, &res); err != nil {
		return Status{}, err
	}
	st := StatePending
	switch res.Transaction.Status {
	case "approved", "transferred":
		st = StateSucceeded
	case "declined", "refunded":
		st = StateFailed
	case "canceled":
		st = StateCancelled
	}
	return Status{Reference: reference, State: st, Amount: res.Transaction.Amount, Currency: res.Transaction.Currency.ISO}, nil
}

// VerifyWebhook : en-tête X-FEDAPAY-SIGNATURE au format "t=<timestamp>,s=<hmac_sha256_hex>",
// signature calculée sur "<timestamp>.<corps>" avec le secret du webhook.
func (f *FedaPay) VerifyWebhook(signature string, body []byte) (string, string, error) {
	var ts, sig string
	for _, part := range strings.Split(signature, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "s":
			sig = v
		}
	}
	if ts == "" || sig == "" || f.WebhookSecret == "" {
		return "", "", ErrBadSignature
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(t, 0)).Abs() > 10*time.Minute {
		return "", "", ErrBadSignature // protège contre le rejeu
	}
	mac := hmac.New(sha256.New, []byte(f.WebhookSecret))
	mac.Write([]byte(ts + "." + string(body)))
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
		return "", "", ErrBadSignature
	}
	var evt struct {
		ID     any    `json:"id"`
		Name   string `json:"name"`
		Entity struct {
			ID int64 `json:"id"`
		} `json:"entity"`
	}
	if err := json.Unmarshal(body, &evt); err != nil || evt.Entity.ID == 0 {
		return "", "", errors.New("événement FedaPay illisible")
	}
	ref := strconv.FormatInt(evt.Entity.ID, 10)
	return fmt.Sprintf("%v:%s:%s", evt.ID, evt.Name, ref), ref, nil
}

func splitName(full string) (string, string) {
	full = strings.TrimSpace(full)
	if i := strings.LastIndex(full, " "); i > 0 {
		return full[:i], full[i+1:]
	}
	return full, full
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
