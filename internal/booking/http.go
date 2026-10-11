package booking

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/phone"
	"busvoyage/internal/segment"
	"busvoyage/internal/store"
)

const (
	tagBookings = "Réservations"
	tagAdmin    = "Admin — Réservations"
)

// ---------------------------------------------------------------- DTO

type BookedSeat struct {
	BookingSeatID     uuid.UUID `json:"booking_seat_id" doc:"Identifiant du passager dans la réservation (bagages)"`
	SeatID            uuid.UUID `json:"seat_id"`
	SeatLabel         string    `json:"seat_label"`
	CategoryCode      string    `json:"category_code"`
	CategoryName      string    `json:"category_name"`
	PassengerName     string    `json:"passenger_name"`
	PassengerPhone    string    `json:"passenger_phone,omitempty"`
	BasePrice         int64     `json:"base_price"`
	CategorySurcharge int64     `json:"category_surcharge"`
	SeatSurcharge     int64     `json:"seat_surcharge"`
	DiscountAmount    int64     `json:"discount_amount" doc:"Réduction liée au niveau de fidélité"`
	TotalPrice        int64     `json:"total_price"`
	Status            string    `json:"status" enum:"held,sold,cancelled,expired"`
}

type BookingTicket struct {
	ID            uuid.UUID  `json:"id"`
	TicketNumber  string     `json:"ticket_number"`
	SeatLabel     string     `json:"seat_label"`
	PassengerName string     `json:"passenger_name"`
	Status        string     `json:"status" enum:"valid,used,cancelled"`
	QRPayload     string     `json:"qr_payload"`
	BoardedAt     *time.Time `json:"boarded_at,omitempty"`
}

type BookingPayment struct {
	ID          uuid.UUID  `json:"id"`
	Provider    string     `json:"provider"`
	Method      string     `json:"method"`
	Amount      int64      `json:"amount"`
	Status      string     `json:"status"`
	CheckoutURL *string    `json:"checkout_url,omitempty"`
	SucceededAt *time.Time `json:"succeeded_at,omitempty"`
}

type DepartureSummary struct {
	ID                uuid.UUID `json:"id"`
	DepartureAt       time.Time `json:"departure_at"`
	ArrivalEta        time.Time `json:"arrival_eta"`
	Status            string    `json:"status"`
	RouteName         string    `json:"route_name"`
	OriginCity        string    `json:"origin_city"`
	OriginAgencyName  string    `json:"origin_agency_name"`
	DestinationCity   string    `json:"destination_city"`
	CompanyName       string    `json:"company_name"`
	IncludedBaggageKg int32     `json:"included_baggage_kg"`
}

type Booking struct {
	ID                  uuid.UUID  `json:"id"`
	Reference           string     `json:"reference"`
	CompanyID           uuid.UUID  `json:"company_id"`
	Status              string     `json:"status" enum:"pending,confirmed,cancelled,expired,refunded,changed"`
	Channel             string     `json:"channel" enum:"app,web,counter"`
	ContactName         string     `json:"contact_name"`
	ContactPhone        string     `json:"contact_phone"`
	ContactEmail        *string    `json:"contact_email,omitempty"`
	SubtotalAmount      int64      `json:"subtotal_amount"`
	DiscountAmount      int64      `json:"discount_amount"`
	LoyaltyPointsUsed   int32      `json:"loyalty_points_used"`
	LoyaltyPointsAmount int64      `json:"loyalty_points_amount"`
	TotalAmount         int64      `json:"total_amount" doc:"Montant à payer"`
	Currency            string     `json:"currency"`
	ExpiresAt           time.Time  `json:"expires_at" doc:"Fin du blocage des sièges si non payé"`
	ConfirmedAt         *time.Time `json:"confirmed_at,omitempty"`
	CancelledAt         *time.Time `json:"cancelled_at,omitempty"`
	CancelReason        string     `json:"cancel_reason,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	DepartureAt         *time.Time `json:"departure_at,omitempty"`
	RouteName           string     `json:"route_name,omitempty"`
	CompanyName         string     `json:"company_name,omitempty"`
	ChangedFromID       *uuid.UUID `json:"changed_from_booking_id,omitempty" doc:"Réservation remplacée par celle-ci (modification)"`
	ChangeCount         int32      `json:"change_count"`
	ChangeFeeAmount     int64      `json:"change_fee_amount" doc:"Frais de modification inclus dans total_amount"`
	ChangeCreditAmount  int64      `json:"change_credit_amount" doc:"Avoir reporté depuis la réservation remplacée"`
	PaymentMode         string     `json:"payment_mode" enum:"online,agency,on_account" doc:"online : paiement immédiat ; agency : à payer en agence avant pay_by"`
	EarlyBirdAmount     int64      `json:"early_bird_amount" doc:"Remise « réservez tôt » (incluse dans discount_amount)"`
	PromoCode           *string    `json:"promo_code,omitempty"`
	PromoAmount         int64      `json:"promo_amount" doc:"Remise du code promo"`
	ReferralAmount      int64      `json:"referral_amount" doc:"Offre de bienvenue (parrainage)"`
	PayBy               *time.Time `json:"pay_by,omitempty" doc:"Date limite de paiement (payer en agence)"`
	BillingName         *string    `json:"billing_name,omitempty" doc:"Facture au nom de cette entreprise"`
	BillingTaxID        *string    `json:"billing_tax_id,omitempty"`
}

// BookingRef : lien vers une autre réservation.
type BookingRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
}

type BookingDetail struct {
	Booking
	Departure   DepartureSummary `json:"departure"`
	Seats       []BookedSeat     `json:"seats"`
	Tickets     []BookingTicket  `json:"tickets"`
	Payments    []BookingPayment `json:"payments"`
	ReplacedBy  *BookingRef      `json:"replaced_by,omitempty" doc:"Nouvelle réservation après une modification"`
	ChangedFrom *BookingRef      `json:"changed_from,omitempty"`
}

func toBooking(b store.Booking) Booking {
	return Booking{ID: b.ID, Reference: b.Reference, CompanyID: b.CompanyID, Status: b.Status, Channel: b.Channel,
		ContactName: b.ContactName, ContactPhone: b.ContactPhone, ContactEmail: b.ContactEmail,
		SubtotalAmount: b.SubtotalAmount, DiscountAmount: b.DiscountAmount, LoyaltyPointsUsed: b.LoyaltyPointsUsed,
		LoyaltyPointsAmount: b.LoyaltyPointsAmount, TotalAmount: b.TotalAmount, Currency: b.Currency,
		ExpiresAt: b.ExpiresAt, ConfirmedAt: b.ConfirmedAt, CancelledAt: b.CancelledAt, CancelReason: b.CancelReason,
		CreatedAt: b.CreatedAt, ChangedFromID: b.ChangedFromBookingID, ChangeCount: b.ChangeCount,
		ChangeFeeAmount: b.ChangeFeeAmount, ChangeCreditAmount: b.ChangeCreditAmount,
		BillingName: b.BillingName, BillingTaxID: b.BillingTaxID, PaymentMode: b.PaymentMode, PayBy: b.PayBy,
		EarlyBirdAmount: b.EarlyBirdAmount, PromoCode: b.PromoCode, PromoAmount: b.PromoAmount, ReferralAmount: b.ReferralAmount}
}

// LoadDetail charge une réservation complète (sièges, billets, paiements).
func LoadDetail(ctx context.Context, q *store.Queries, id uuid.UUID) (BookingDetail, error) {
	b, err := q.GetBooking(ctx, id)
	if err != nil {
		return BookingDetail{}, err
	}
	out := BookingDetail{Booking: toBooking(b), Seats: []BookedSeat{}, Tickets: []BookingTicket{}, Payments: []BookingPayment{}}
	d, err := q.GetDepartureDetail(ctx, b.DepartureID)
	if err != nil {
		return out, err
	}
	// Trajet du voyageur (arrêt de montée → arrêt de descente), pas les extrémités de la ligne.
	if err := segment.ForBooking(ctx, q, &d, b.ID); err != nil {
		return out, err
	}
	out.Departure = DepartureSummary{ID: d.ID, DepartureAt: d.DepartureAt, ArrivalEta: d.ArrivalEta, Status: d.Status,
		RouteName: d.RouteName, OriginCity: d.OriginCity, OriginAgencyName: d.OriginAgencyName,
		DestinationCity: d.DestinationCity, CompanyName: d.CompanyName, IncludedBaggageKg: d.IncludedBaggageKg}
	if b.Status == "changed" {
		if r, err := q.GetReplacementBooking(ctx, &b.ID); err == nil {
			out.ReplacedBy = &BookingRef{ID: r.ID, Reference: r.Reference}
		} else if err := ignoreNotFound(err); err != nil {
			return out, err
		}
	}
	if b.ChangedFromBookingID != nil {
		if o, err := q.GetBooking(ctx, *b.ChangedFromBookingID); err == nil {
			out.ChangedFrom = &BookingRef{ID: o.ID, Reference: o.Reference}
		} else if err := ignoreNotFound(err); err != nil {
			return out, err
		}
	}
	seats, err := q.ListBookingSeats(ctx, b.ID)
	if err != nil {
		return out, err
	}
	for _, s := range seats {
		out.Seats = append(out.Seats, BookedSeat{BookingSeatID: s.ID, SeatID: s.SeatCellID, SeatLabel: s.SeatLabel, CategoryCode: s.CategoryCode,
			CategoryName: s.CategoryName, PassengerName: s.PassengerName, PassengerPhone: s.PassengerPhone,
			BasePrice: s.BasePrice, CategorySurcharge: s.CategorySurcharge, SeatSurcharge: s.SeatSurcharge,
			DiscountAmount: s.DiscountAmount, TotalPrice: s.TotalPrice, Status: s.Status})
	}
	tickets, err := q.ListBookingTickets(ctx, b.ID)
	if err != nil {
		return out, err
	}
	for _, t := range tickets {
		out.Tickets = append(out.Tickets, BookingTicket{ID: t.ID, TicketNumber: t.TicketNumber, SeatLabel: t.SeatLabel,
			PassengerName: t.PassengerName, Status: t.Status, QRPayload: t.QrPayload, BoardedAt: t.BoardedAt})
	}
	payments, err := q.ListBookingPayments(ctx, &b.ID)
	if err != nil {
		return out, err
	}
	for _, p := range payments {
		out.Payments = append(out.Payments, BookingPayment{ID: p.ID, Provider: p.Provider, Method: p.Method, Amount: p.Amount,
			Status: p.Status, CheckoutURL: p.CheckoutUrl, SucceededAt: p.SucceededAt})
	}
	return out, nil
}

// ---------------------------------------------------------------- Routes

type createOutput struct {
	Status int
	Body   BookingDetail
}

func (s *Service) Register(api huma.API) {
	s.registerPayLater(api)
	huma.Register(api, apix.WithDescription(apix.Op("create-booking", http.MethodPost, "/v1/bookings", tagBookings,
		"Réserver (bloque les sièges pendant le paiement)", apix.Customer),
		"Parcours : choisir les sièges (ou laisser l'attribution automatique), puis payer avant `expires_at`. "+
			"Le prix est toujours calculé par le serveur : tarif de base + supplément de catégorie + supplément du siège "+
			"− réduction de niveau de fidélité − points utilisés. Réponse 409 si un siège vient d'être pris."),
		func(ctx context.Context, in *struct {
			ClientType string `header:"X-Client-Type" enum:"web,mobile" default:"mobile"`
			Body       CreateInput
		}) (*createOutput, error) {
			p := auth.MustFromContext(ctx)
			channel := "app"
			if in.ClientType == "web" {
				channel = "web"
			}
			d, err := s.CreateForCustomer(ctx, p, channel, in.Body)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &createOutput{Status: http.StatusCreated, Body: d}, nil
		})

	huma.Register(api, apix.Op("list-my-bookings", http.MethodGet, "/v1/bookings", tagBookings, "Mes réservations", apix.Customer),
		func(ctx context.Context, in *struct {
			apix.Page
			Status string `query:"status" enum:"pending,confirmed,cancelled,expired,refunded,changed,"`
		}) (*struct{ Body []Booking }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Booking }{Body: []Booking{}}
			err := s.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCustomerBookings(ctx, store.ListCustomerBookingsParams{
					CustomerID: &p.ID, Status: apix.NilIfEmpty(in.Status), Lim: in.Limit, Off: in.Offset,
				})
				for _, r := range rows {
					b := toBooking(store.Booking{ID: r.ID, CompanyID: r.CompanyID, Reference: r.Reference, DepartureID: r.DepartureID,
						CustomerID: r.CustomerID, Channel: r.Channel, Status: r.Status, ContactName: r.ContactName,
						ContactPhone: r.ContactPhone, ContactEmail: r.ContactEmail, SubtotalAmount: r.SubtotalAmount,
						DiscountAmount: r.DiscountAmount, LoyaltyPointsUsed: r.LoyaltyPointsUsed, LoyaltyPointsAmount: r.LoyaltyPointsAmount,
						TotalAmount: r.TotalAmount, Currency: r.Currency, ExpiresAt: r.ExpiresAt, ConfirmedAt: r.ConfirmedAt,
						CancelledAt: r.CancelledAt, CancelReason: r.CancelReason, CreatedAt: r.CreatedAt})
					dep := r.DepartureAt
					b.DepartureAt, b.RouteName, b.CompanyName = &dep, r.RouteName, r.CompanyName
					out.Body = append(out.Body, b)
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-my-booking", http.MethodGet, "/v1/bookings/{id}", tagBookings, "Détail d'une réservation", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body BookingDetail }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body BookingDetail }{}
			err := s.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				b, err := q.GetBooking(ctx, in.ID)
				if err != nil || b.CustomerID == nil || *b.CustomerID != p.ID {
					return apix.NotFound("Réservation")
				}
				out.Body, err = LoadDetail(ctx, q, b.ID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("cancel-my-booking", http.MethodPost, "/v1/bookings/{id}/cancel", tagBookings,
		"Annuler ma réservation", apix.Customer),
		"Réservation non payée : sièges libérés immédiatement. Réservation payée : possible jusqu'au délai fixé par la compagnie ; "+
			"une demande de remboursement (moins les frais d'annulation) est créée et les points utilisés sont restitués."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Reason string `json:"reason,omitempty" maxLength:"300"`
			}
		}) (*struct{ Body BookingDetail }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body BookingDetail }{}
			err := s.DB.Tx(ctx, database.Customer(), func(tx pgx.Tx, q *store.Queries) error {
				b, err := q.GetBookingForUpdate(ctx, in.ID)
				if err != nil || b.CustomerID == nil || *b.CustomerID != p.ID {
					return apix.NotFound("Réservation")
				}
				reason := in.Body.Reason
				if reason == "" {
					reason = "Annulée par le client"
				}
				if err := s.Cancel(ctx, tx, q, b, CancelOptions{Reason: reason}); err != nil {
					return err
				}
				if err := apix.Audit(ctx, q, &b.CompanyID, "booking.cancelled", "booking", b.ID.String(),
					map[string]any{"status": b.Status}, map[string]any{"status": "cancelled", "reason": reason}); err != nil {
					return err
				}
				out.Body, err = LoadDetail(ctx, q, b.ID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("get-my-booking-policy", http.MethodGet, "/v1/bookings/{id}/policy", tagBookings,
		"Conditions d'annulation et de modification", apix.Customer),
		"Calculées pour cette réservation : règles de la compagnie, avantages du niveau de fidélité, délais, frais et montant remboursable."),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body BookingPolicy }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body BookingPolicy }{}
			err := s.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				b, err := q.GetBooking(ctx, in.ID)
				if err != nil || b.CustomerID == nil || *b.CustomerID != p.ID {
					return apix.NotFound("Réservation")
				}
				out.Body, err = s.Policy(ctx, q, b, time.Now())
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("list-change-departures", http.MethodGet, "/v1/bookings/{id}/change/departures", tagBookings,
		"Départs possibles pour modifier ma réservation", apix.Customer),
		"Même compagnie, mêmes villes, assez de places pour tous les passagers, dans la fenêtre autorisée."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Date string    `query:"date" required:"true" format:"date" doc:"Jour recherché (AAAA-MM-JJ, heure du Bénin)"`
		}) (*struct{ Body []ChangeCandidate }, error) {
			day, err := time.ParseInLocation(time.DateOnly, in.Date, localTZ)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("Date invalide")
			}
			out, err := s.ChangeCandidates(ctx, auth.MustFromContext(ctx), in.ID, day)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body []ChangeCandidate }{Body: out}, nil
		})

	type changeBody struct {
		DepartureID uuid.UUID `json:"departure_id"`
	}

	huma.Register(api, apix.WithDescription(apix.Op("quote-booking-change", http.MethodPost, "/v1/bookings/{id}/change/quote", tagBookings,
		"Prix d'une modification", apix.Customer),
		"Simule la modification sans rien réserver : nouveau tarif, avoir reporté, frais et montant à payer."),
		func(ctx context.Context, in *struct {
			ID         uuid.UUID `path:"id"`
			ClientType string    `header:"X-Client-Type" enum:"web,mobile" default:"mobile"`
			Body       changeBody
		}) (*struct{ Body ChangeQuote }, error) {
			q, err := s.QuoteChange(ctx, auth.MustFromContext(ctx), clientChannel(in.ClientType), in.ID, in.Body.DepartureID)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body ChangeQuote }{Body: q}, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("change-my-booking", http.MethodPost, "/v1/bookings/{id}/change", tagBookings,
		"Modifier ma réservation (changer de départ)", apix.Customer),
		"Crée une nouvelle réservation sur le départ choisi, avec les mêmes passagers. S'il reste un montant à payer "+
			"(supplément + frais), elle est en attente de paiement et l'ancienne reste valable jusque-là. "+
			"Sinon elle est confirmée immédiatement. L'ancienne passe alors au statut `changed`."),
		func(ctx context.Context, in *struct {
			ID         uuid.UUID `path:"id"`
			ClientType string    `header:"X-Client-Type" enum:"web,mobile" default:"mobile"`
			Body       changeBody
		}) (*createOutput, error) {
			d, err := s.Change(ctx, auth.MustFromContext(ctx), clientChannel(in.ClientType), in.ID, in.Body.DepartureID)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &createOutput{Status: http.StatusCreated, Body: d}, nil
		})

	// --- Personnel
	sellers := []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier}

	type staffChangeBody struct {
		DepartureID uuid.UUID  `json:"departure_id"`
		WaiveFee    bool       `json:"waive_fee,omitempty" doc:"Offrir les frais de modification"`
		AgencyID    *uuid.UUID `json:"agency_id,omitempty" doc:"Agence d'encaissement (par défaut celle de l'agent)"`
	}

	huma.Register(api, apix.Op("get-booking-policy", http.MethodGet, "/v1/admin/bookings/{id}/policy", tagAdmin,
		"Conditions d'annulation et de modification (vue guichet)", apix.Staff, sellers...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body BookingPolicy }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body BookingPolicy }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				b, err := q.GetBooking(ctx, in.ID)
				if err != nil || b.CompanyID != p.CompanyID {
					return apix.NotFound("Réservation")
				}
				out.Body, err = s.StaffPolicy(ctx, q, b, time.Now())
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-booking-change-departures", http.MethodGet, "/v1/admin/bookings/{id}/change/departures", tagAdmin,
		"Départs possibles pour modifier une réservation (guichet)", apix.Staff, sellers...),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Date string    `query:"date" required:"true" format:"date"`
		}) (*struct{ Body []ChangeCandidate }, error) {
			day, err := time.ParseInLocation(time.DateOnly, in.Date, localTZ)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("Date invalide")
			}
			out, err := s.changeCandidates(ctx, changeActor{principal: auth.MustFromContext(ctx), staff: true}, in.ID, day)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body []ChangeCandidate }{Body: out}, nil
		})

	huma.Register(api, apix.Op("quote-booking-change-counter", http.MethodPost, "/v1/admin/bookings/{id}/change/quote", tagAdmin,
		"Prix d'une modification (guichet)", apix.Staff, sellers...),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body staffChangeBody
		}) (*struct{ Body ChangeQuote }, error) {
			a, err := s.counterActor(ctx, in.Body.AgencyID, in.Body.WaiveFee)
			if err != nil {
				return nil, apix.Err(err)
			}
			q, err := s.quoteChange(ctx, a, in.ID, in.Body.DepartureID)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body ChangeQuote }{Body: q}, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("change-booking-counter", http.MethodPost, "/v1/admin/bookings/{id}/change", tagAdmin,
		"Modifier une réservation au guichet", apix.Staff, sellers...),
		"Jusqu'au départ, sans limite de nombre. Le montant dû (différence + frais, frais offerts si `waive_fee`) "+
			"est encaissé en espèces et la nouvelle réservation est confirmée immédiatement : les nouveaux billets sont émis, "+
			"les anciens annulés."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body staffChangeBody
		}) (*createOutput, error) {
			a, err := s.counterActor(ctx, in.Body.AgencyID, in.Body.WaiveFee)
			if err != nil {
				return nil, apix.Err(err)
			}
			d, err := s.applyChange(ctx, a, in.ID, in.Body.DepartureID)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &createOutput{Status: http.StatusCreated, Body: d}, nil
		})

	huma.Register(api, apix.Op("list-bookings", http.MethodGet, "/v1/admin/bookings", tagAdmin, "Rechercher des réservations", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleFinance),
		func(ctx context.Context, in *struct {
			apix.Page
			DepartureID uuid.UUID `query:"departure_id"`
			AgencyID    uuid.UUID `query:"agency_id"`
			Status      string    `query:"status" enum:"pending,confirmed,cancelled,expired,refunded,changed,"`
			PaymentMode string    `query:"payment_mode" enum:"online,agency,on_account," doc:"agency : réservations à encaisser en agence"`
			Q           string    `query:"q" maxLength:"30" doc:"Référence de réservation ou numéro de téléphone"`
		}) (*struct{ Body []Booking }, error) {
			p := auth.MustFromContext(ctx)
			query := strings.TrimSpace(in.Q)
			// Un numéro saisi librement (01 97 00 00 00) est cherché au format international.
			if strings.ContainsAny(query, "0123456789") && !strings.ContainsAny(strings.ToUpper(query), "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
				if n, err := phone.Normalize(query, s.Cfg.DefaultCountryCode); err == nil {
					query = n
				}
			}
			params := store.ListCompanyBookingsParams{CompanyID: p.CompanyID, Status: apix.NilIfEmpty(in.Status),
				PaymentMode: apix.NilIfEmpty(in.PaymentMode), Q: apix.NilIfEmpty(query), Lim: in.Limit, Off: in.Offset}
			if in.DepartureID != uuid.Nil {
				params.DepartureID = &in.DepartureID
			}
			if in.AgencyID != uuid.Nil {
				params.AgencyID = &in.AgencyID
			}
			out := &struct{ Body []Booking }{Body: []Booking{}}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCompanyBookings(ctx, params)
				for _, r := range rows {
					b := toBooking(store.Booking{ID: r.ID, CompanyID: r.CompanyID, Reference: r.Reference, Status: r.Status,
						Channel: r.Channel, ContactName: r.ContactName, ContactPhone: r.ContactPhone, ContactEmail: r.ContactEmail,
						SubtotalAmount: r.SubtotalAmount, DiscountAmount: r.DiscountAmount, LoyaltyPointsUsed: r.LoyaltyPointsUsed,
						LoyaltyPointsAmount: r.LoyaltyPointsAmount, TotalAmount: r.TotalAmount, Currency: r.Currency,
						ExpiresAt: r.ExpiresAt, ConfirmedAt: r.ConfirmedAt, CancelledAt: r.CancelledAt, CancelReason: r.CancelReason,
						CreatedAt: r.CreatedAt, PaymentMode: r.PaymentMode, PayBy: r.PayBy})
					dep := r.DepartureAt
					b.DepartureAt, b.RouteName = &dep, r.RouteName
					out.Body = append(out.Body, b)
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-booking", http.MethodGet, "/v1/admin/bookings/{id}", tagAdmin, "Détail d'une réservation", apix.Staff),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body BookingDetail }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body BookingDetail }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, err = LoadDetail(ctx, q, in.ID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-counter-booking", http.MethodPost, "/v1/admin/bookings", tagAdmin,
		"Vente au guichet (paiement espèces)", apix.Staff, sellers...),
		"Crée et confirme immédiatement la réservation avec un encaissement en espèces rattaché à l'agence et au caissier. "+
			"Le client est identifié par son téléphone : ses voyages comptent pour la fidélité. "+
			"Le siège disparaît instantanément de l'application mobile et du site."),
		func(ctx context.Context, in *struct {
			Body struct {
				CreateInput
				AgencyID *uuid.UUID `json:"agency_id,omitempty" doc:"Par défaut : l'agence de l'employé"`
			}
		}) (*createOutput, error) {
			p := auth.MustFromContext(ctx)
			in.Body.RedeemPoints = 0
			d, err := s.CreateAtCounter(ctx, p, in.Body.AgencyID, in.Body.CreateInput)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &createOutput{Status: http.StatusCreated, Body: d}, nil
		})

	huma.Register(api, apix.Op("cancel-booking", http.MethodPost, "/v1/admin/bookings/{id}/cancel", tagAdmin, "Annuler une réservation", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Reason   string `json:"reason" minLength:"3" maxLength:"300"`
				WaiveFee bool   `json:"waive_fee,omitempty" doc:"Rembourser sans frais d'annulation"`
			}
		}) (*struct{ Body BookingDetail }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body BookingDetail }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
				b, err := q.GetBookingForUpdate(ctx, in.ID)
				if database.IsNotFound(err) {
					return apix.NotFound("Réservation")
				}
				if err != nil {
					return err
				}
				if err := s.Cancel(ctx, tx, q, b, CancelOptions{Reason: in.Body.Reason, ByStaff: true, WaiveFee: in.Body.WaiveFee}); err != nil {
					return err
				}
				if err := apix.Audit(ctx, q, &p.CompanyID, "booking.cancelled", "booking", b.ID.String(),
					map[string]any{"status": b.Status}, map[string]any{"status": "cancelled", "reason": in.Body.Reason, "waive_fee": in.Body.WaiveFee}); err != nil {
					return err
				}
				out.Body, err = LoadDetail(ctx, q, b.ID)
				return err
			})
			return out, apix.Err(err)
		})
}

func clientChannel(clientType string) string {
	if clientType == "web" {
		return "web"
	}
	return "app"
}

// counterActor : agent au guichet, avec contrôle de l'agence d'encaissement
// (mêmes règles que la vente au guichet).
func (s *Service) counterActor(ctx context.Context, agencyID *uuid.UUID, waiveFee bool) (changeActor, error) {
	p := auth.MustFromContext(ctx)
	if agencyID == nil {
		agencyID = p.AgencyID
	}
	if agencyID == nil {
		return changeActor{}, huma.Error422UnprocessableEntity("Agence d'encaissement requise (agency_id)")
	}
	if p.Role != auth.RoleCompanyAdmin && (p.AgencyID == nil || *p.AgencyID != *agencyID) {
		return changeActor{}, huma.Error403Forbidden("Vous ne pouvez encaisser que pour votre agence")
	}
	err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		if _, err := q.GetAgency(ctx, store.GetAgencyParams{ID: *agencyID, CompanyID: p.CompanyID}); err != nil {
			if database.IsNotFound(err) {
				return huma.Error422UnprocessableEntity("Agence inconnue")
			}
			return err
		}
		return nil
	})
	return changeActor{principal: p, channel: "counter", staff: true, agencyID: agencyID, waiveFee: waiveFee}, err
}
