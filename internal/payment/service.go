package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/booking"
	"busvoyage/internal/config"
	"busvoyage/internal/invoice"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/phone"
	"busvoyage/internal/store"
)

const (
	tagPayments = "Paiements"
	tagRefunds  = "Admin — Remboursements"
	tagDev      = "Développement"
)

type Service struct {
	DB        *database.DB
	Cfg       config.Config
	Bookings  *booking.Service
	Providers map[string]Provider
}

// ---------------------------------------------------------------- DTO

type Payment struct {
	ID            uuid.UUID  `json:"id"`
	BookingID     uuid.UUID  `json:"booking_id"`
	Provider      string     `json:"provider"`
	Method        string     `json:"method"`
	Amount        int64      `json:"amount"`
	Currency      string     `json:"currency"`
	Status        string     `json:"status" enum:"pending,succeeded,failed,cancelled,refunded"`
	CheckoutURL   *string    `json:"checkout_url,omitempty" doc:"Page de paiement du fournisseur vers laquelle rediriger le client"`
	BookingStatus string     `json:"booking_status,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	SucceededAt   *time.Time `json:"succeeded_at,omitempty"`
}

func toPayment(p store.Payment) Payment {
	return Payment{ID: p.ID, BookingID: bookingOf(p), Provider: p.Provider, Method: p.Method, Amount: p.Amount,
		Currency: p.Currency, Status: p.Status, CheckoutURL: p.CheckoutUrl, CreatedAt: p.CreatedAt, SucceededAt: p.SucceededAt}
}

// ---------------------------------------------------------------- Logique

// apply applique un statut confirmé par le fournisseur. Idempotent.
func (s *Service) apply(ctx context.Context, tx pgx.Tx, q *store.Queries, pay store.Payment, st Status) (store.Payment, error) {
	if pay.Status != "pending" {
		return pay, nil // déjà traité
	}
	switch st.State {
	case StateSucceeded:
		// Contrôle du montant : une transaction d'un autre montant n'est jamais acceptée.
		if st.Amount != 0 && (st.Amount != pay.Amount || (st.Currency != "" && st.Currency != pay.Currency)) {
			slog.Error("montant de paiement incohérent", "payment", pay.ID, "expected", pay.Amount, "got", st.Amount)
			return q.MarkPaymentFailed(ctx, store.MarkPaymentFailedParams{ID: pay.ID, Status: "failed",
				FailureReason: fmt.Sprintf("Montant reçu %d %s ≠ attendu %d %s", st.Amount, st.Currency, pay.Amount, pay.Currency)})
		}
		updated, err := q.MarkPaymentSucceeded(ctx, pay.ID)
		if err != nil {
			return pay, err
		}
		if pay.BookingID != nil { // les colis sont payés en espèces, jamais en ligne
			if _, err := s.Bookings.ConfirmPaid(ctx, tx, q, *pay.BookingID); err != nil {
				return pay, err
			}
		}
		return updated, nil
	case StateFailed, StateCancelled:
		return q.MarkPaymentFailed(ctx, store.MarkPaymentFailedParams{ID: pay.ID, Status: string(st.State), FailureReason: "Refusé ou annulé chez le fournisseur"})
	}
	return pay, nil
}

// Initiate démarre un paiement pour une réservation du client.
func (s *Service) Initiate(ctx context.Context, p auth.Principal, bookingID uuid.UUID, providerName, method, payerPhone, idemKey string) (Payment, error) {
	owns := func(b store.Booking) bool { return b.CustomerID != nil && *b.CustomerID == p.ID }
	return s.initiate(ctx, database.Customer(), owns, "cust:"+p.ID.String()+":", nil, bookingID, providerName, method, payerPhone, idemKey)
}

// InitiateForStaff : un agent lance le paiement en ligne d'une réservation de sa compagnie
// (voyageur au guichet ou au téléphone, qui confirme sur son propre téléphone).
func (s *Service) InitiateForStaff(ctx context.Context, p auth.Principal, bookingID uuid.UUID, providerName, method, payerPhone, idemKey string) (Payment, error) {
	owns := func(b store.Booking) bool {
		// Contrôleur : seulement ses ventes à bord.
		if p.Role == auth.RoleBoardingAgent {
			return b.CompanyID == p.CompanyID && b.CreatedByStaffID != nil && *b.CreatedByStaffID == p.ID
		}
		return b.CompanyID == p.CompanyID
	}
	staffID := p.ID
	return s.initiate(ctx, p.Tenant(), owns, "staff:"+p.ID.String()+":", &staffID, bookingID, providerName, method, payerPhone, idemKey)
}

func (s *Service) initiate(ctx context.Context, tenant database.Tenant, owns func(store.Booking) bool, keyPrefix string, staffID *uuid.UUID,
	bookingID uuid.UUID, providerName, method, payerPhone, idemKey string) (Payment, error) {
	prov, ok := s.Providers[providerName]
	if !ok {
		return Payment{}, huma.Error422UnprocessableEntity("Moyen de paiement indisponible : " + providerName)
	}
	var (
		pay      store.Payment
		existing bool
		b        store.Booking
	)
	err := s.DB.Tx(ctx, tenant, func(_ pgx.Tx, q *store.Queries) error {
		var err error
		b, err = q.GetBookingForUpdate(ctx, bookingID)
		if err != nil || !owns(b) {
			return apix.NotFound("Réservation")
		}
		key := keyPrefix + idemKey
		if prev, err := q.GetPaymentByIdempotencyKey(ctx, key); err == nil {
			if prev.BookingID == nil || *prev.BookingID != b.ID {
				return huma.Error409Conflict("Clé d'idempotence déjà utilisée pour une autre réservation")
			}
			pay, existing = prev, true
			return nil
		} else if !database.IsNotFound(err) {
			return err
		}
		if b.Status != "pending" {
			return huma.Error409Conflict("Cette réservation n'est pas en attente de paiement (statut : " + b.Status + ")")
		}
		if time.Now().After(b.ExpiresAt) {
			return huma.Error409Conflict("Le délai de paiement est dépassé : refaites une réservation")
		}
		if b.TotalAmount <= 0 {
			return huma.Error409Conflict("Aucun montant à payer")
		}
		pay, err = q.InsertPayment(ctx, store.InsertPaymentParams{
			CompanyID: b.CompanyID, BookingID: &b.ID, Provider: providerName, Method: method, Amount: b.TotalAmount,
			Currency: b.Currency, PayerPhone: payerPhone, IdempotencyKey: key, ReceivedByStaffID: staffID,
		})
		return err
	})
	if err != nil {
		return Payment{}, err
	}
	if existing || pay.CheckoutUrl != nil {
		return toPayment(pay), nil
	}

	// Appel au fournisseur hors transaction (latence réseau).
	res, err := prov.Initiate(ctx, InitiateRequest{
		PaymentID: pay.ID, Amount: pay.Amount, Currency: pay.Currency,
		Description:  "Réservation " + b.Reference,
		CustomerName: b.ContactName, CustomerPhone: payerPhone, CustomerEmail: deref(b.ContactEmail),
		CallbackURL: s.Cfg.PublicWebURL + "/reservation/" + b.ID.String() + "/confirmation?paiement=" + pay.ID.String(),
	})
	if err != nil {
		slog.Error("initialisation du paiement", "provider", providerName, "err", err)
		_ = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
			_, err := q.MarkPaymentFailed(ctx, store.MarkPaymentFailedParams{ID: pay.ID, Status: "failed", FailureReason: "Initialisation impossible"})
			return err
		})
		return Payment{}, huma.Error502BadGateway("Le service de paiement est indisponible, réessayez")
	}
	err = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		pay, err = q.SetPaymentCheckout(ctx, store.SetPaymentCheckoutParams{ID: pay.ID, ProviderReference: &res.ProviderReference, CheckoutUrl: &res.CheckoutURL})
		return err
	})
	return toPayment(pay), err
}

// Reconcile interroge le fournisseur pour un paiement en attente (utile si le webhook tarde).
func (s *Service) Reconcile(ctx context.Context, paymentID uuid.UUID) (store.Payment, error) {
	var pay store.Payment
	err := s.DB.Tx(ctx, database.System(), func(tx pgx.Tx, q *store.Queries) error {
		var err error
		pay, err = q.GetPaymentForUpdate(ctx, paymentID)
		if err != nil || pay.Status != "pending" || pay.ProviderReference == nil {
			return err
		}
		prov, ok := s.Providers[pay.Provider]
		if !ok {
			return nil
		}
		st, err := prov.FetchStatus(ctx, *pay.ProviderReference)
		if err != nil {
			slog.Warn("vérification du statut de paiement", "payment", pay.ID, "err", err)
			return nil
		}
		pay, err = s.apply(ctx, tx, q, pay, st)
		return err
	})
	return pay, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------- Routes

// ProviderInfo décrit un moyen de paiement proposé au client.
type ProviderInfo struct {
	Code    string   `json:"code"`
	Label   string   `json:"label"`
	Methods []string `json:"methods" doc:"mobile_money, card"`
}

var providerLabels = map[string]ProviderInfo{
	"fedapay":  {Label: "Mobile Money ou carte (FedaPay)", Methods: []string{"mobile_money", "card"}},
	"kkiapay":  {Label: "Mobile Money ou carte (KKiaPay)", Methods: []string{"mobile_money", "card"}},
	"cinetpay": {Label: "Mobile Money ou carte (CinetPay)", Methods: []string{"mobile_money", "card"}},
	"mock":     {Label: "Paiement simulé (développement)", Methods: []string{"mobile_money", "card"}},
}

func (s *Service) Register(api huma.API) {
	huma.Register(api, apix.Op("list-payment-providers", http.MethodGet, "/v1/public/payment-providers", tagPayments,
		"Moyens de paiement disponibles", apix.Public),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []ProviderInfo }, error) {
			out := &struct{ Body []ProviderInfo }{Body: []ProviderInfo{}}
			for _, name := range s.Cfg.PaymentProviders {
				info := providerLabels[name]
				info.Code = name
				out.Body = append(out.Body, info)
			}
			return out, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-payment", http.MethodPost, "/v1/bookings/{id}/payments", tagPayments,
		"Payer une réservation", apix.Customer),
		"Renvoie `checkout_url` : rediriger le client vers la page du fournisseur (Mobile Money, carte). "+
			"L'en-tête `Idempotency-Key` évite les doubles paiements en cas de nouvelle tentative réseau. "+
			"La confirmation arrive par webhook ; l'application peut interroger `GET /v1/payments/{id}`."),
		func(ctx context.Context, in *struct {
			ID             uuid.UUID `path:"id"`
			IdempotencyKey string    `header:"Idempotency-Key" required:"true" minLength:"8" maxLength:"100"`
			Body           struct {
				Provider   string `json:"provider" doc:"Fournisseur actif (ex. fedapay ; mock en développement)"`
				Method     string `json:"method,omitempty" enum:"mobile_money,card" default:"mobile_money"`
				PayerPhone string `json:"payer_phone,omitempty" maxLength:"20" doc:"Numéro Mobile Money"`
			}
		}) (*struct {
			Status int
			Body   Payment
		}, error) {
			p := auth.MustFromContext(ctx)
			payer := in.Body.PayerPhone
			if payer != "" {
				var err error
				if payer, err = phone.Normalize(payer, s.Cfg.DefaultCountryCode); err != nil {
					return nil, huma.Error422UnprocessableEntity("Numéro Mobile Money invalide")
				}
			}
			pay, err := s.Initiate(ctx, p, in.ID, in.Body.Provider, in.Body.Method, payer, in.IdempotencyKey)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct {
				Status int
				Body   Payment
			}{Status: http.StatusCreated, Body: pay}, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-staff-payment", http.MethodPost, "/v1/admin/bookings/{id}/payments", tagPayments,
		"Lancer le paiement en ligne d'une réservation (guichet, téléphone, vente à bord)", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleBoardingAgent),
		"L'agent saisit le numéro Mobile Money du voyageur sur la page du fournisseur (`checkout_url`, ouverte sur son écran) : "+
			"le voyageur confirme avec son code sur son propre téléphone, même un téléphone simple. Confirmation par webhook ; "+
			"suivre le statut avec `GET /v1/admin/payments/{id}`."),
		func(ctx context.Context, in *struct {
			ID             uuid.UUID `path:"id"`
			IdempotencyKey string    `header:"Idempotency-Key" required:"true" minLength:"8" maxLength:"100"`
			Body           struct {
				Provider   string `json:"provider" doc:"Fournisseur actif (ex. fedapay)"`
				Method     string `json:"method,omitempty" enum:"mobile_money,card" default:"mobile_money"`
				PayerPhone string `json:"payer_phone,omitempty" maxLength:"20" doc:"Numéro Mobile Money du voyageur"`
			}
		}) (*struct {
			Status int
			Body   Payment
		}, error) {
			p := auth.MustFromContext(ctx)
			payer := in.Body.PayerPhone
			if payer != "" {
				var err error
				if payer, err = phone.Normalize(payer, s.Cfg.DefaultCountryCode); err != nil {
					return nil, huma.Error422UnprocessableEntity("Numéro Mobile Money invalide")
				}
			}
			pay, err := s.InitiateForStaff(ctx, p, in.ID, in.Body.Provider, in.Body.Method, payer, in.IdempotencyKey)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct {
				Status int
				Body   Payment
			}{Status: http.StatusCreated, Body: pay}, nil
		})

	huma.Register(api, apix.Op("get-staff-payment", http.MethodGet, "/v1/admin/payments/{id}", tagPayments,
		"Statut d'un paiement (vérifié auprès du fournisseur)", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleFinance, auth.RoleBoardingAgent),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body Payment }, error) {
			p := auth.MustFromContext(ctx)
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				pay, err := q.GetPaymentForUpdate(ctx, in.ID)
				if err != nil || pay.CompanyID != p.CompanyID {
					return apix.NotFound("Paiement")
				}
				return nil
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			pay, err := s.Reconcile(ctx, in.ID)
			if err != nil {
				return nil, apix.Err(err)
			}
			out := &struct{ Body Payment }{Body: toPayment(pay)}
			_ = s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				b, err := q.GetBooking(ctx, bookingOf(pay))
				out.Body.BookingStatus = b.Status
				return err
			})
			return out, nil
		})

	huma.Register(api, apix.Op("get-payment", http.MethodGet, "/v1/payments/{id}", tagPayments,
		"Statut d'un paiement (avec vérification auprès du fournisseur)", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body Payment }, error) {
			p := auth.MustFromContext(ctx)
			var owner *uuid.UUID
			err := s.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				pay, err := q.GetPaymentForUpdate(ctx, in.ID)
				if err != nil || pay.BookingID == nil {
					return apix.NotFound("Paiement")
				}
				b, err := q.GetBooking(ctx, *pay.BookingID)
				owner = b.CustomerID
				return err
			})
			if err != nil || owner == nil || *owner != p.ID {
				return nil, apix.Err(apix.NotFound("Paiement"))
			}
			pay, err := s.Reconcile(ctx, in.ID)
			if err != nil {
				return nil, apix.Err(err)
			}
			out := &struct{ Body Payment }{Body: toPayment(pay)}
			_ = s.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				b, err := q.GetBooking(ctx, bookingOf(pay))
				out.Body.BookingStatus = b.Status
				return err
			})
			return out, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("payment-webhook", http.MethodPost, "/v1/webhooks/payments/{provider}", tagPayments,
		"Notification d'un fournisseur de paiement", apix.Public),
		"Signature vérifiée (HMAC), protection contre le rejeu, idempotence par identifiant d'événement. "+
			"Le statut est ensuite relu auprès de l'API du fournisseur avant toute confirmation."),
		func(ctx context.Context, in *struct {
			Provider  string `path:"provider"`
			Signature string `header:"X-FEDAPAY-SIGNATURE"`
			RawBody   []byte
		}) (*struct{}, error) {
			prov, ok := s.Providers[in.Provider]
			if !ok {
				return nil, huma.Error404NotFound("Fournisseur inconnu")
			}
			eventID, ref, err := prov.VerifyWebhook(in.Signature, in.RawBody)
			if err != nil {
				slog.Warn("webhook rejeté", "provider", in.Provider, "err", err, "ip", apix.Meta(ctx).IP)
				return nil, huma.Error401Unauthorized("Signature invalide")
			}
			payload := json.RawMessage(in.RawBody)
			if !json.Valid(payload) {
				payload = json.RawMessage(`{}`)
			}
			err = s.DB.Tx(ctx, database.System(), func(tx pgx.Tx, q *store.Queries) error {
				evID, err := q.InsertPaymentEvent(ctx, store.InsertPaymentEventParams{Provider: in.Provider, EventID: eventID, Payload: payload})
				if database.IsNotFound(err) {
					return nil // événement déjà reçu
				}
				if err != nil {
					return err
				}
				pay, err := q.GetPaymentByProviderRefForUpdate(ctx, store.GetPaymentByProviderRefForUpdateParams{Provider: in.Provider, ProviderReference: &ref})
				if database.IsNotFound(err) {
					slog.Warn("webhook pour une transaction inconnue", "provider", in.Provider, "ref", ref)
					return q.MarkPaymentEventProcessed(ctx, evID)
				}
				if err != nil {
					return err
				}
				st, err := prov.FetchStatus(ctx, ref)
				if err != nil {
					return err // le fournisseur renverra le webhook
				}
				if _, err := s.apply(ctx, tx, q, pay, st); err != nil {
					return err
				}
				return q.MarkPaymentEventProcessed(ctx, evID)
			})
			return nil, apix.Err(err)
		})

	if s.Cfg.AllowsSimulation() {
		huma.Register(api, apix.WithDescription(apix.Op("simulate-payment", http.MethodPost, "/v1/dev/payments/{id}/simulate", tagDev,
			"Simuler le résultat d'un paiement (fournisseur mock)", apix.Public),
			"**Disponible uniquement en développement et en recette (staging).**"),
			func(ctx context.Context, in *struct {
				ID   uuid.UUID `path:"id"`
				Body struct {
					Outcome string `json:"outcome,omitempty" enum:"succeeded,failed" default:"succeeded"`
				}
			}) (*struct{ Body Payment }, error) {
				out := &struct{ Body Payment }{}
				err := s.DB.Tx(ctx, database.System(), func(tx pgx.Tx, q *store.Queries) error {
					pay, err := q.GetPaymentForUpdate(ctx, in.ID)
					if err != nil {
						return err
					}
					if pay.Provider != "mock" {
						return huma.Error422UnprocessableEntity("Seuls les paiements mock peuvent être simulés")
					}
					pay, err = s.apply(ctx, tx, q, pay, Status{State: State(in.Body.Outcome), Amount: pay.Amount, Currency: pay.Currency})
					out.Body = toPayment(pay)
					return err
				})
				return out, apix.Err(err)
			})
	}

	s.registerRefunds(api)
	s.registerCash(api)
}

// ---------------------------------------------------------------- Remboursements

type Refund struct {
	ID               uuid.UUID  `json:"id"`
	BookingID        uuid.UUID  `json:"booking_id"`
	BookingReference string     `json:"booking_reference,omitempty"`
	PaymentID        *uuid.UUID `json:"payment_id,omitempty"`
	Amount           int64      `json:"amount"`
	Reason           string     `json:"reason"`
	Status           string     `json:"status" enum:"requested,approved,rejected,processed"`
	RequestedByType  string     `json:"requested_by_type"`
	RequestedBy      *uuid.UUID `json:"requested_by,omitempty"`
	ApprovedBy       *uuid.UUID `json:"approved_by,omitempty"`
	DecidedAt        *time.Time `json:"decided_at,omitempty"`
	ProcessedAt      *time.Time `json:"processed_at,omitempty"`
	ProcessMethod    *string    `json:"process_method,omitempty" doc:"Mode de versement (mobile_money, cash…)"`
	ProcessedAgency  *uuid.UUID `json:"processed_agency_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

func toRefund(r store.Refund) Refund {
	return Refund{ID: r.ID, BookingID: r.BookingID, PaymentID: r.PaymentID, Amount: r.Amount, Reason: r.Reason, Status: r.Status,
		RequestedByType: r.RequestedByType, RequestedBy: r.RequestedBy, ApprovedBy: r.ApprovedBy, DecidedAt: r.DecidedAt,
		ProcessedAt: r.ProcessedAt, ProcessMethod: r.ProcessMethod, ProcessedAgency: r.ProcessedAgencyID, CreatedAt: r.CreatedAt}
}

func (s *Service) registerRefunds(api huma.API) {
	deciders := []string{auth.RoleCompanyAdmin, auth.RoleFinance, auth.RoleAgencyManager}

	huma.Register(api, apix.Op("list-refunds", http.MethodGet, "/v1/admin/refunds", tagRefunds, "Lister les remboursements", apix.Staff, deciders...),
		func(ctx context.Context, in *struct {
			apix.Page
			Status string `query:"status" enum:"requested,approved,rejected,processed,"`
		}) (*struct{ Body []Refund }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Refund }{Body: []Refund{}}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListRefunds(ctx, store.ListRefundsParams{CompanyID: p.CompanyID, Status: apix.NilIfEmpty(in.Status), Lim: in.Limit, Off: in.Offset})
				for _, r := range rows {
					rf := toRefund(store.Refund{ID: r.ID, BookingID: r.BookingID, PaymentID: r.PaymentID, Amount: r.Amount, Reason: r.Reason,
						Status: r.Status, RequestedByType: r.RequestedByType, RequestedBy: r.RequestedBy, ApprovedBy: r.ApprovedBy,
						DecidedAt: r.DecidedAt, ProcessedAt: r.ProcessedAt, CreatedAt: r.CreatedAt})
					rf.BookingReference = r.BookingReference
					out.Body = append(out.Body, rf)
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("decide-refund", http.MethodPost, "/v1/admin/refunds/{id}/decision", tagRefunds,
		"Approuver ou refuser un remboursement", apix.Staff, deciders...)),
		"Contrôle anti-fraude : au-delà du seuil fixé par la compagnie, seuls `company_admin` ou `finance` peuvent approuver, "+
			"et jamais la personne qui a demandé le remboursement (principe des quatre yeux)."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Approve bool   `json:"approve"`
				Comment string `json:"comment,omitempty" maxLength:"300"`
			}
		}) (*struct{ Body Refund }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body Refund }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				r, err := q.GetRefundForUpdate(ctx, store.GetRefundForUpdateParams{ID: in.ID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return apix.NotFound("Remboursement")
				}
				if err != nil {
					return err
				}
				if r.Status != "requested" {
					return huma.Error409Conflict("Ce remboursement a déjà été traité")
				}
				company, err := q.GetCompany(ctx, p.CompanyID)
				if err != nil {
					return err
				}
				if in.Body.Approve && r.Amount > company.RefundApprovalThreshold {
					if !p.HasRole(auth.RoleCompanyAdmin, auth.RoleFinance) {
						return huma.Error403Forbidden("Montant supérieur au seuil : validation par un administrateur ou la finance requise")
					}
					if r.RequestedBy != nil && *r.RequestedBy == p.ID {
						return huma.Error403Forbidden("Vous ne pouvez pas approuver un remboursement que vous avez demandé")
					}
				}
				status := "rejected"
				if in.Body.Approve {
					status = "approved"
				}
				staffID := p.ID
				updated, err := q.DecideRefund(ctx, store.DecideRefundParams{ID: r.ID, Status: status, ApprovedBy: &staffID})
				if err != nil {
					return err
				}
				if in.Body.Approve {
					if _, err := q.SetBookingStatus(ctx, store.SetBookingStatusParams{ID: r.BookingID, Status: "refunded", CancelReason: r.Reason}); err != nil {
						return err
					}
				}
				out.Body = toRefund(updated)
				return apix.Audit(ctx, q, &p.CompanyID, "refund."+status, "refund", r.ID.String(),
					toRefund(r), map[string]any{"status": status, "comment": in.Body.Comment})
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("mark-refund-processed", http.MethodPost, "/v1/admin/refunds/{id}/processed", tagRefunds,
		"Marquer un remboursement comme versé", apix.Staff, auth.RoleCompanyAdmin, auth.RoleFinance)),
		"À utiliser une fois l'argent renvoyé au client. Un versement en espèces est déduit de la caisse de l'agence "+
			"(clôture de caisse) : `agency_id` est alors obligatoire, sauf si l'agent est rattaché à une agence."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body *struct {
				Method   string     `json:"method" enum:"mobile_money,cash,bank_transfer,other" default:"mobile_money"`
				AgencyID *uuid.UUID `json:"agency_id,omitempty" doc:"Agence qui a versé les espèces"`
			} `required:"false"`
		}) (*struct{ Body Refund }, error) {
			p := auth.MustFromContext(ctx)
			method, agencyID := "mobile_money", (*uuid.UUID)(nil)
			if in.Body != nil {
				method, agencyID = in.Body.Method, in.Body.AgencyID
			}
			if method == "cash" && agencyID == nil {
				agencyID = p.AgencyID
			}
			if method == "cash" && agencyID == nil {
				return nil, huma.Error422UnprocessableEntity("Indiquez l'agence qui a versé les espèces")
			}
			if method != "cash" {
				agencyID = nil
			}
			out := &struct{ Body Refund }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
				if agencyID != nil {
					if _, err := q.GetAgency(ctx, store.GetAgencyParams{ID: *agencyID, CompanyID: p.CompanyID}); err != nil {
						if database.IsNotFound(err) {
							return huma.Error422UnprocessableEntity("Agence inconnue")
						}
						return err
					}
				}
				r, err := q.GetRefundForUpdate(ctx, store.GetRefundForUpdateParams{ID: in.ID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return apix.NotFound("Remboursement")
				}
				if err != nil {
					return err
				}
				if r.Status != "approved" {
					return huma.Error409Conflict("Seul un remboursement approuvé peut être marqué comme versé")
				}
				staffID := p.ID
				updated, err := q.MarkRefundProcessed(ctx, store.MarkRefundProcessedParams{ID: r.ID, ProcessMethod: &method,
					ProcessedAgencyID: agencyID, ProcessedBy: &staffID})
				if err != nil {
					return err
				}
				out.Body = toRefund(updated)
				invoice.ForRefund(ctx, tx, q, updated, p.ID)
				return apix.Audit(ctx, q, &p.CompanyID, "refund.processed", "refund", r.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})
}

// bookingOf : réservation payée (zéro pour un paiement de colis).
func bookingOf(p store.Payment) uuid.UUID {
	if p.BookingID == nil {
		return uuid.Nil
	}
	return *p.BookingID
}
