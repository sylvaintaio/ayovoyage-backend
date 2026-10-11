package parcel

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/invoice"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/refgen"
	"busvoyage/internal/segment"
	"busvoyage/internal/store"
)

const tagBaggage = "Bagages"

var baggageRoles = []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleBoardingAgent, auth.RoleDriver}

type Baggage struct {
	ID               uuid.UUID  `json:"id"`
	TagNumber        string     `json:"tag_number"`
	BookingID        uuid.UUID  `json:"booking_id"`
	BookingReference string     `json:"booking_reference,omitempty"`
	BookingSeatID    uuid.UUID  `json:"booking_seat_id"`
	PassengerName    string     `json:"passenger_name"`
	SeatLabel        string     `json:"seat_label"`
	DepartureID      uuid.UUID  `json:"departure_id"`
	WeightKg         float64    `json:"weight_kg"`
	Description      string     `json:"description"`
	ExtraFee         int64      `json:"extra_fee" doc:"Excédent encaissé"`
	Status           string     `json:"status" enum:"checked,loaded,unloaded,delivered,missing,cancelled"`
	LoadedAt         *time.Time `json:"loaded_at,omitempty"`
	UnloadedAt       *time.Time `json:"unloaded_at,omitempty"`
	DeliveredAt      *time.Time `json:"delivered_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

func toBaggage(t store.BaggageTag, passenger, seat, booking string) Baggage {
	return Baggage{ID: t.ID, TagNumber: t.TagNumber, BookingID: t.BookingID, BookingReference: booking, BookingSeatID: t.BookingSeatID,
		PassengerName: passenger, SeatLabel: seat, DepartureID: t.DepartureID, WeightKg: t.WeightKg, Description: t.Description,
		ExtraFee: t.ExtraFee, Status: t.Status, LoadedAt: t.LoadedAt, UnloadedAt: t.UnloadedAt, DeliveredAt: t.DeliveredAt, CreatedAt: t.CreatedAt}
}

// Transitions autorisées par scan.
var baggageTransitions = map[string]map[string]string{
	"load":    {"checked": "loaded"},
	"unload":  {"loaded": "unloaded"},
	"deliver": {"unloaded": "delivered", "loaded": "delivered"},
	"missing": {"checked": "missing", "loaded": "missing", "unloaded": "missing"},
	"found":   {"missing": "unloaded"},
}

func (s *Service) registerBaggage(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("check-baggage", http.MethodPost, "/v1/admin/bookings/{id}/baggage", tagBaggage,
		"Enregistrer un bagage en soute", apix.Staff, baggageRoles...),
		"Génère une étiquette QR liée au passager, à la réservation et au départ. Au-delà du poids inclus sur la ligne "+
			"(cumulé par passager), l'excédent est facturé au kilo (paramètre de la compagnie) et encaissé en espèces, sauf `waive_fee`."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				BookingSeatID uuid.UUID  `json:"booking_seat_id"`
				WeightKg      float64    `json:"weight_kg" minimum:"0.1" maximum:"200"`
				Description   string     `json:"description,omitempty" maxLength:"200" doc:"Ex. valise noire, sac de riz"`
				WaiveFee      bool       `json:"waive_fee,omitempty"`
				AgencyID      *uuid.UUID `json:"agency_id,omitempty" doc:"Agence d'encaissement de l'excédent"`
			}
		}) (*struct {
			Status int
			Body   Baggage
		}, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			var out Baggage
			err := s.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
				bk, err := q.GetBookingForUpdate(ctx, in.ID)
				if err != nil || bk.CompanyID != p.CompanyID {
					return apix.NotFound("Réservation")
				}
				if bk.Status != "confirmed" {
					return huma.Error409Conflict("La réservation doit être confirmée")
				}
				seats, err := q.ListBookingSeats(ctx, bk.ID)
				if err != nil {
					return err
				}
				var seat *store.ListBookingSeatsRow
				for i := range seats {
					if seats[i].ID == b.BookingSeatID && seats[i].Status == "sold" {
						seat = &seats[i]
					}
				}
				if seat == nil {
					return huma.Error422UnprocessableEntity("Passager inconnu pour cette réservation")
				}
				dep, err := q.GetDepartureDetail(ctx, bk.DepartureID)
				if err != nil {
					return err
				}
				// Trajet du voyageur (arrêt de montée → arrêt de descente), pas les extrémités de la ligne.
				if err := segment.ForBooking(ctx, q, &dep, bk.ID); err != nil {
					return err
				}
				// Possible tant que le car n'a pas quitté l'arrêt de montée du voyageur.
				if err := segment.OpenForSale(ctx, q, store.Departure{ID: dep.ID, Status: dep.Status}, int(bk.BoardingSeq)); err != nil {
					return err
				}
				company, err := q.GetCompany(ctx, p.CompanyID)
				if err != nil {
					return err
				}
				already := 0.0
				existing, err := q.ListBookingBaggage(ctx, bk.ID)
				if err != nil {
					return err
				}
				for _, t := range existing {
					if t.BookingSeatID == seat.ID {
						already += t.WeightKg
					}
				}
				fee := int64(0)
				if over := already + b.WeightKg - float64(dep.IncludedBaggageKg); over > 0 && !b.WaiveFee {
					fee = int64(math.Ceil(math.Min(over, b.WeightKg))) * company.BaggageExtraPerKg
				}
				staffID := p.ID
				tag, err := q.InsertBaggageTag(ctx, store.InsertBaggageTagParams{CompanyID: p.CompanyID, TagNumber: refgen.Baggage(),
					BookingID: bk.ID, BookingSeatID: seat.ID, DepartureID: bk.DepartureID, WeightKg: b.WeightKg,
					Description: strings.TrimSpace(b.Description), ExtraFee: fee, CreatedBy: &staffID})
				if err != nil {
					return err
				}
				if fee > 0 {
					agencyID, err := staffAgency(p, b.AgencyID)
					if err != nil {
						return err
					}
					pay, err := q.InsertPayment(ctx, store.InsertPaymentParams{CompanyID: p.CompanyID, BookingID: &bk.ID, Provider: "cash",
						Method: "cash", Amount: fee, Currency: bk.Currency, PayerPhone: bk.ContactPhone,
						IdempotencyKey: "cash:baggage:" + tag.ID.String(), AgencyID: &agencyID, ReceivedByStaffID: &staffID})
					if err != nil {
						return err
					}
					paid, err := q.MarkPaymentSucceeded(ctx, pay.ID)
					if err != nil {
						return err
					}
					overKg := math.Min(already+b.WeightKg-float64(dep.IncludedBaggageKg), b.WeightKg)
					invoice.ForBaggage(ctx, tx, q, paid, bk, tag.TagNumber, seat.PassengerName, math.Ceil(overKg))
				}
				out = toBaggage(tag, seat.PassengerName, seat.SeatLabel, bk.Reference)
				return apix.Audit(ctx, q, &p.CompanyID, "baggage.checked", "booking", bk.ID.String(), nil,
					map[string]any{"tag": tag.TagNumber, "weight": b.WeightKg, "extra_fee": fee, "waived": b.WaiveFee})
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct {
				Status int
				Body   Baggage
			}{Status: http.StatusCreated, Body: out}, nil
		})

	list := func(ctx context.Context, tenant database.Tenant, fn func(q *store.Queries) ([]Baggage, error)) ([]Baggage, error) {
		var out []Baggage
		err := s.DB.Tx(ctx, tenant, func(_ pgx.Tx, q *store.Queries) error {
			var err error
			out, err = fn(q)
			return err
		})
		return out, err
	}

	huma.Register(api, apix.Op("list-booking-baggage", http.MethodGet, "/v1/admin/bookings/{id}/baggage", tagBaggage,
		"Bagages d'une réservation", apix.Staff, baggageRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body []Baggage }, error) {
			p := auth.MustFromContext(ctx)
			out, err := list(ctx, p.Tenant(), func(q *store.Queries) ([]Baggage, error) {
				rows, err := q.ListBookingBaggage(ctx, in.ID)
				res := []Baggage{}
				for _, r := range rows {
					if r.CompanyID == p.CompanyID {
						res = append(res, toBaggage(store.BaggageTag{ID: r.ID, CompanyID: r.CompanyID, TagNumber: r.TagNumber, BookingID: r.BookingID,
							BookingSeatID: r.BookingSeatID, DepartureID: r.DepartureID, WeightKg: r.WeightKg, Description: r.Description,
							ExtraFee: r.ExtraFee, Status: r.Status, LoadedAt: r.LoadedAt, UnloadedAt: r.UnloadedAt, DeliveredAt: r.DeliveredAt,
							CreatedAt: r.CreatedAt}, r.PassengerName, r.SeatLabel, ""))
					}
				}
				return res, err
			})
			return &struct{ Body []Baggage }{Body: out}, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-my-booking-baggage", http.MethodGet, "/v1/bookings/{id}/baggage", tagBaggage,
		"Mes bagages enregistrés", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body []Baggage }, error) {
			p := auth.MustFromContext(ctx)
			out, err := list(ctx, database.System(), func(q *store.Queries) ([]Baggage, error) {
				bk, err := q.GetBooking(ctx, in.ID)
				if err != nil || bk.CustomerID == nil || *bk.CustomerID != p.ID {
					return nil, apix.NotFound("Réservation")
				}
				rows, err := q.ListBookingBaggage(ctx, bk.ID)
				res := []Baggage{}
				for _, r := range rows {
					res = append(res, Baggage{ID: r.ID, TagNumber: r.TagNumber, BookingID: r.BookingID, BookingSeatID: r.BookingSeatID,
						PassengerName: r.PassengerName, SeatLabel: r.SeatLabel, DepartureID: r.DepartureID, WeightKg: r.WeightKg,
						Description: r.Description, ExtraFee: r.ExtraFee, Status: r.Status, LoadedAt: r.LoadedAt, UnloadedAt: r.UnloadedAt,
						DeliveredAt: r.DeliveredAt, CreatedAt: r.CreatedAt})
				}
				return res, err
			})
			return &struct{ Body []Baggage }{Body: out}, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-departure-baggage", http.MethodGet, "/v1/admin/departures/{id}/baggage", tagBaggage,
		"Manifeste bagages d'un départ", apix.Staff, baggageRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body []Baggage }, error) {
			p := auth.MustFromContext(ctx)
			out, err := list(ctx, p.Tenant(), func(q *store.Queries) ([]Baggage, error) {
				rows, err := q.ListDepartureBaggage(ctx, in.ID)
				res := []Baggage{}
				for _, r := range rows {
					res = append(res, Baggage{ID: r.ID, TagNumber: r.TagNumber, BookingID: r.BookingID, BookingReference: r.BookingReference,
						BookingSeatID: r.BookingSeatID, PassengerName: r.PassengerName, SeatLabel: r.SeatLabel, DepartureID: r.DepartureID,
						WeightKg: r.WeightKg, Description: r.Description, ExtraFee: r.ExtraFee, Status: r.Status, LoadedAt: r.LoadedAt,
						UnloadedAt: r.UnloadedAt, DeliveredAt: r.DeliveredAt, CreatedAt: r.CreatedAt})
				}
				return res, err
			})
			return &struct{ Body []Baggage }{Body: out}, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("scan-baggage", http.MethodPost, "/v1/admin/baggage/scan", tagBaggage,
		"Scanner un bagage", apix.Staff, baggageRoles...),
		"`load` au chargement (refusé si le bagage est d'un autre départ), `unload` à l'arrivée, `deliver` à la remise au "+
			"passager, `missing` s'il manque (une réclamation est ouverte), `found` s'il est retrouvé."),
		func(ctx context.Context, in *struct {
			Body struct {
				Tag         string     `json:"tag" maxLength:"40"`
				Action      string     `json:"action" enum:"load,unload,deliver,missing,found"`
				DepartureID *uuid.UUID `json:"departure_id,omitempty" doc:"Départ en cours de chargement (contrôle)"`
			}
		}) (*struct{ Body Baggage }, error) {
			p := auth.MustFromContext(ctx)
			var out Baggage
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				t, err := q.GetBaggageByTagForUpdate(ctx, strings.TrimSpace(in.Body.Tag))
				if err != nil || t.CompanyID != p.CompanyID {
					return apix.NotFound("Bagage")
				}
				if in.Body.DepartureID != nil && *in.Body.DepartureID != t.DepartureID {
					return huma.Error409Conflict("Ce bagage voyage sur un autre départ : ne pas le charger")
				}
				next, ok := baggageTransitions[in.Body.Action][t.Status]
				if !ok {
					if t.Status == map[string]string{"load": "loaded", "unload": "unloaded", "deliver": "delivered", "missing": "missing"}[in.Body.Action] {
						next = t.Status // scan répété
					} else {
						return huma.Error409Conflict(fmt.Sprintf("Action impossible : bagage %s", baggageStatusText[t.Status]))
					}
				}
				if next != t.Status {
					if t, err = q.SetBaggageStatus(ctx, store.SetBaggageStatusParams{ID: t.ID, Status: next}); err != nil {
						return err
					}
					if next == "missing" {
						v, err := q.GetBaggageView(ctx, t.ID)
						if err != nil {
							return err
						}
						staffID := p.ID
						if _, err := s.openIncident(ctx, q, newIncident{companyID: t.CompanyID, subject: "baggage", baggageID: &t.ID,
							category: "lost", description: "Bagage " + t.TagNumber + " (" + t.Description + ") signalé manquant par l'équipe.",
							reporterType: "staff", reporterName: v.PassengerName, reporterPhone: v.ContactPhone, priority: "urgent", staffID: &staffID}); err != nil {
							return err
						}
					}
				}
				v, err := q.GetBaggageView(ctx, t.ID)
				if err != nil {
					return err
				}
				out = toBaggage(t, v.PassengerName, v.SeatLabel, v.BookingReference)
				return nil
			})
			return &struct{ Body Baggage }{Body: out}, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-baggage-label", http.MethodGet, "/v1/admin/baggage/{id}/label", tagBaggage,
		"Étiquette bagage (PDF)", apix.Staff, baggageRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*pdfOutput, error) {
			p := auth.MustFromContext(ctx)
			var v store.GetBaggageViewRow
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				v, err = q.GetBaggageView(ctx, in.ID)
				if err != nil || v.CompanyID != p.CompanyID {
					return apix.NotFound("Bagage")
				}
				return nil
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			pdf, err := baggageLabel(v.CompanyName, v.TagNumber, v.PassengerName, v.SeatLabel, v.RouteName, v.DestinationAgencyName,
				v.DepartureAt.In(localTZ).Format("02/01/2006 15h04"), v.BookingReference, v.WeightKg)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &pdfOutput{ContentType: "application/pdf", CacheControl: "private, no-store",
				ContentDisposition: fmt.Sprintf(`inline; filename="bagage-%s.pdf"`, v.TagNumber), Body: pdf}, nil
		})
}

var baggageStatusText = map[string]string{
	"checked": "enregistré, pas encore chargé", "loaded": "déjà chargé", "unloaded": "déjà déchargé",
	"delivered": "déjà remis au passager", "missing": "signalé manquant", "cancelled": "annulé",
}
