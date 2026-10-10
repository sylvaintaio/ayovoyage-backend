package parcel

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

const tagPublicParcels = "Public — Colis"

// PublicTracking : suivi visible par tous avec le numéro, sans données personnelles.
type PublicTracking struct {
	TrackingNumber     string        `json:"tracking_number"`
	Status             string        `json:"status"`
	StatusLabel        string        `json:"status_label"`
	CompanyName        string        `json:"company_name"`
	OriginCity         string        `json:"origin_city"`
	OriginAgency       string        `json:"origin_agency"`
	DestinationCity    string        `json:"destination_city"`
	DestinationAgency  string        `json:"destination_agency"`
	DestinationAddress string        `json:"destination_address"`
	DestinationPhone   string        `json:"destination_phone,omitempty"`
	CurrentAgency      string        `json:"current_agency,omitempty"`
	SenderName         string        `json:"sender_name" doc:"Abrégé (ex. Awa D.)"`
	RecipientName      string        `json:"recipient_name" doc:"Abrégé"`
	Pieces             int32         `json:"pieces"`
	AmountDue          int64         `json:"amount_due" doc:"Port dû restant à régler au retrait"`
	CreatedAt          time.Time     `json:"created_at"`
	DeliveredAt        *time.Time    `json:"delivered_at,omitempty"`
	Steps              []PublicEvent `json:"steps"`
}

type PublicEvent struct {
	Type      string    `json:"type"`
	Label     string    `json:"label"`
	Place     string    `json:"place,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// shortName : « Awa Dossou » → « Awa D. »
func shortName(full string) string {
	parts := strings.Fields(full)
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	if len(parts) > 1 {
		r, _ := utf8.DecodeRuneInString(parts[len(parts)-1])
		out += " " + strings.ToUpper(string(r)) + "."
	}
	return out
}

// Étapes internes non montrées au public.
var hiddenEvents = map[string]bool{"unassigned": true, "left_behind": true, "incident": true}

func (s *Service) publicTracking(ctx context.Context, q *store.Queries, tracking string) (PublicTracking, store.GetParcelViewRow, error) {
	id, err := q.GetParcelIDByTracking(ctx, TrackingFromScan(tracking))
	if err != nil {
		return PublicTracking{}, store.GetParcelViewRow{}, apix.NotFound("Colis")
	}
	v, err := q.GetParcelView(ctx, id)
	if err != nil {
		return PublicTracking{}, v, err
	}
	out := PublicTracking{TrackingNumber: v.TrackingNumber, Status: v.Status, StatusLabel: statusLabels[v.Status], CompanyName: v.CompanyName,
		OriginCity: v.OriginCity, OriginAgency: v.OriginAgencyName, DestinationCity: v.DestinationCity,
		DestinationAgency: v.DestinationAgencyName, DestinationAddress: v.DestinationAddress, DestinationPhone: v.DestinationPhone,
		CurrentAgency: deref(v.CurrentAgencyName), SenderName: shortName(v.SenderName), RecipientName: shortName(v.RecipientName),
		Pieces: v.Pieces, CreatedAt: v.CreatedAt, DeliveredAt: v.DeliveredAt, Steps: []PublicEvent{}}
	if v.Payer == "recipient" && v.PaidAt == nil {
		out.AmountDue = v.Price
	}
	evs, err := q.ListParcelEvents(ctx, v.ID)
	for _, e := range evs {
		if hiddenEvents[e.EventType] {
			continue
		}
		place := deref(e.AgencyName)
		out.Steps = append(out.Steps, PublicEvent{Type: e.EventType, Label: eventLabels[e.EventType], Place: place, CreatedAt: e.CreatedAt})
	}
	return out, v, err
}

type ParcelReportInput struct {
	Phone       string   `json:"phone" minLength:"8" maxLength:"20" doc:"Téléphone de l'expéditeur ou du destinataire (vérification)"`
	Name        string   `json:"name" minLength:"2" maxLength:"120"`
	Category    string   `json:"category" enum:"not_received,delayed,damaged,opened,wrong_recipient,wrong_agency,info_error,lost,other"`
	Description string   `json:"description" minLength:"10" maxLength:"2000"`
	Photos      []string `json:"photos,omitempty" maxItems:"3" doc:"Photos (data URL JPEG/PNG, 2 Mo chacune au plus)"`
}

func (s *Service) registerPublic(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("track-parcel", http.MethodGet, "/v1/public/parcels/{tracking}", tagPublicParcels,
		"Suivre un colis", apix.Public),
		"Accepte le numéro de suivi ou le contenu du QR code de l'étiquette. Les noms sont abrégés et aucun téléphone n'est exposé."),
		func(ctx context.Context, in *struct {
			Tracking string `path:"tracking" maxLength:"40"`
		}) (*struct {
			CacheControl string `header:"Cache-Control"`
			Body         PublicTracking
		}, error) {
			out := &struct {
				CacheControl string `header:"Cache-Control"`
				Body         PublicTracking
			}{CacheControl: "no-store"}
			err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, _, err = s.publicTracking(ctx, q, in.Tracking)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("report-parcel-problem", http.MethodPost, "/v1/public/parcels/{tracking}/incidents", tagPublicParcels,
		"Signaler un problème sur un colis", apix.Public),
		"Le téléphone doit être celui de l'expéditeur ou du destinataire. Renvoie la référence du dossier, à suivre sur "+
			"`GET /v1/public/incidents/{reference}`."),
		func(ctx context.Context, in *struct {
			Tracking string `path:"tracking" maxLength:"40"`
			Body     ParcelReportInput
		}) (*struct {
			Status int
			Body   IncidentCreated
		}, error) {
			ph, err := s.normPhone(in.Body.Phone, "contact")
			if err != nil {
				return nil, err
			}
			var customerID *uuid.UUID
			if p, ok := auth.FromContext(ctx); ok && p.IsCustomer() {
				customerID = &p.ID
			}
			var created IncidentCreated
			err = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				id, err := q.GetParcelIDByTracking(ctx, TrackingFromScan(in.Tracking))
				if err != nil {
					return apix.NotFound("Colis")
				}
				parcel, err := q.GetParcelForUpdate(ctx, id)
				if err != nil {
					return err
				}
				if ph != parcel.SenderPhone && ph != parcel.RecipientPhone {
					return huma.Error403Forbidden("Ce numéro ne correspond ni à l'expéditeur ni au destinataire du colis")
				}
				created, err = s.openIncident(ctx, q, newIncident{companyID: parcel.CompanyID, subject: "parcel", parcelID: &parcel.ID,
					category: in.Body.Category, description: in.Body.Description, reporterType: "public", reporterName: in.Body.Name,
					reporterPhone: ph, customerID: customerID, photos: in.Body.Photos})
				if err != nil {
					return err
				}
				return s.event(ctx, q, parcel, "incident", parcel.CurrentAgencyID, nil, nil, "Réclamation "+created.Reference)
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct {
				Status int
				Body   IncidentCreated
			}{Status: http.StatusCreated, Body: created}, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("get-public-incident", http.MethodGet, "/v1/public/incidents/{reference}", tagPublicParcels,
		"Suivre une réclamation", apix.Public),
		"Avec le téléphone utilisé lors du signalement. Seuls les échanges publics sont visibles."),
		func(ctx context.Context, in *struct {
			Reference string `path:"reference" maxLength:"20"`
			Phone     string `query:"phone" required:"true" maxLength:"20"`
		}) (*struct{ Body PublicIncident }, error) {
			ph, err := s.normPhone(in.Phone, "contact")
			if err != nil {
				return nil, err
			}
			out := &struct{ Body PublicIncident }{}
			err = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				id, err := q.GetIncidentIDByReference(ctx, strings.TrimSpace(in.Reference))
				if err != nil {
					return apix.NotFound("Réclamation")
				}
				i, err := q.GetIncident(ctx, id)
				if err != nil || i.ReporterPhone != ph {
					return apix.NotFound("Réclamation")
				}
				out.Body, err = s.publicIncident(ctx, q, i)
				return err
			})
			return out, apix.Err(err)
		})

	// --- Espace client
	huma.Register(api, apix.Op("list-my-parcels", http.MethodGet, "/v1/me/parcels", tagPublicParcels, "Mes colis (envoyés et reçus)", apix.Customer),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []CustomerParcel }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []CustomerParcel }{Body: []CustomerParcel{}}
			err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCustomerParcels(ctx, p.ID)
				for _, r := range rows {
					out.Body = append(out.Body, CustomerParcel{TrackingNumber: r.TrackingNumber, Status: r.Status, StatusLabel: statusLabels[r.Status],
						Direction: map[bool]string{true: "sent", false: "received"}[r.IsSender], SenderName: r.SenderName, RecipientName: r.RecipientName,
						OriginCity: r.OriginCity, DestinationCity: r.DestinationCity, CompanyName: r.CompanyName, CreatedAt: r.CreatedAt, DeliveredAt: r.DeliveredAt})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-my-incidents", http.MethodGet, "/v1/me/incidents", tagPublicParcels, "Mes réclamations", apix.Customer),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []IncidentSummary }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []IncidentSummary }{Body: []IncidentSummary{}}
			err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCustomerIncidents(ctx, &p.ID)
				for _, r := range rows {
					out.Body = append(out.Body, IncidentSummary{ID: r.ID, Reference: r.Reference, SubjectType: r.SubjectType, Category: r.Category,
						Status: r.Status, TrackingNumber: r.TrackingNumber, BookingReference: r.BookingReference, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("report-trip-problem", http.MethodPost, "/v1/me/incidents", tagPublicParcels,
		"Signaler un problème sur un voyage ou un bagage", apix.Customer),
		"Pour une réservation du client (bagage perdu ou endommagé, problème pendant le voyage). Pour un colis, utiliser le suivi du colis."),
		func(ctx context.Context, in *struct {
			Body struct {
				BookingID   uuid.UUID `json:"booking_id"`
				BaggageTag  string    `json:"baggage_tag,omitempty" maxLength:"20" doc:"Étiquette du bagage concerné"`
				Category    string    `json:"category" enum:"not_received,delayed,damaged,opened,lost,other"`
				Description string    `json:"description" minLength:"10" maxLength:"2000"`
				Photos      []string  `json:"photos,omitempty" maxItems:"3"`
			}
		}) (*struct {
			Status int
			Body   IncidentCreated
		}, error) {
			p := auth.MustFromContext(ctx)
			var created IncidentCreated
			err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				b, err := q.GetBooking(ctx, in.Body.BookingID)
				if err != nil || b.CustomerID == nil || *b.CustomerID != p.ID {
					return apix.NotFound("Réservation")
				}
				c, err := q.GetCustomer(ctx, p.ID)
				if err != nil {
					return err
				}
				ni := newIncident{companyID: b.CompanyID, subject: "booking", bookingID: &b.ID, category: in.Body.Category,
					description: in.Body.Description, reporterType: "customer", reporterName: c.FullName,
					reporterPhone: b.ContactPhone, customerID: &p.ID, photos: in.Body.Photos}
				if tag := strings.TrimSpace(in.Body.BaggageTag); tag != "" {
					t, err := q.GetBaggageByTagForUpdate(ctx, tag)
					if err != nil || t.BookingID != b.ID {
						return huma.Error422UnprocessableEntity("Étiquette de bagage inconnue pour cette réservation")
					}
					ni.subject, ni.bookingID, ni.baggageID = "baggage", nil, &t.ID
				}
				created, err = s.openIncident(ctx, q, ni)
				return err
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct {
				Status int
				Body   IncidentCreated
			}{Status: http.StatusCreated, Body: created}, nil
		})

	huma.Register(api, apix.Op("get-my-incident", http.MethodGet, "/v1/me/incidents/{id}", tagPublicParcels, "Détail d'une réclamation", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body PublicIncident }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body PublicIncident }{}
			err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				i, err := q.GetIncident(ctx, in.ID)
				if err != nil || i.ReporterCustomerID == nil || *i.ReporterCustomerID != p.ID {
					return apix.NotFound("Réclamation")
				}
				out.Body, err = s.publicIncident(ctx, q, i)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("comment-my-incident", http.MethodPost, "/v1/me/incidents/{id}/comments", tagPublicParcels,
		"Ajouter un message à ma réclamation", apix.Customer),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Message string `json:"message" minLength:"2" maxLength:"2000"`
			}
		}) (*struct{ Body PublicIncident }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body PublicIncident }{}
			err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				i, err := q.GetIncident(ctx, in.ID)
				if err != nil || i.ReporterCustomerID == nil || *i.ReporterCustomerID != p.ID {
					return apix.NotFound("Réclamation")
				}
				if i.Status == "resolved" || i.Status == "rejected" {
					return huma.Error409Conflict("Ce dossier est clos")
				}
				if err := q.InsertIncidentEvent(ctx, store.InsertIncidentEventParams{CompanyID: i.CompanyID, IncidentID: i.ID,
					AuthorType: "customer", AuthorID: &p.ID, AuthorName: i.ReporterName, Action: "comment", Note: in.Body.Message, IsPublic: true}); err != nil {
					return err
				}
				out.Body, err = s.publicIncident(ctx, q, i)
				return err
			})
			return out, apix.Err(err)
		})
}

type CustomerParcel struct {
	TrackingNumber  string     `json:"tracking_number"`
	Status          string     `json:"status"`
	StatusLabel     string     `json:"status_label"`
	Direction       string     `json:"direction" enum:"sent,received"`
	SenderName      string     `json:"sender_name"`
	RecipientName   string     `json:"recipient_name"`
	OriginCity      string     `json:"origin_city"`
	DestinationCity string     `json:"destination_city"`
	CompanyName     string     `json:"company_name"`
	CreatedAt       time.Time  `json:"created_at"`
	DeliveredAt     *time.Time `json:"delivered_at,omitempty"`
}
