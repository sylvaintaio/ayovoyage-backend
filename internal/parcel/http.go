package parcel

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

const tagParcels = "Colis"

// ---------------------------------------------------------------- DTO

type ParcelTariffDTO struct {
	Category         string  `json:"category" enum:"document,small,medium,large,fragile"`
	Label            string  `json:"label" minLength:"2" maxLength:"60"`
	BasePrice        int64   `json:"base_price" minimum:"0"`
	IncludedKg       float64 `json:"included_kg" minimum:"0" maximum:"500"`
	PricePerKg       int64   `json:"price_per_kg" minimum:"0"`
	MaxWeightKg      float64 `json:"max_weight_kg" minimum:"0.1" maximum:"500"`
	InsurancePercent float64 `json:"insurance_percent" minimum:"0" maximum:"100" doc:"Assurance, en % de la valeur déclarée"`
}

type ParcelSummary struct {
	ID                    uuid.UUID  `json:"id"`
	TrackingNumber        string     `json:"tracking_number"`
	Status                string     `json:"status"`
	SenderName            string     `json:"sender_name"`
	SenderPhone           string     `json:"sender_phone"`
	RecipientName         string     `json:"recipient_name"`
	RecipientPhone        string     `json:"recipient_phone"`
	Category              string     `json:"category"`
	WeightKg              float64    `json:"weight_kg"`
	Pieces                int32      `json:"pieces"`
	Price                 int64      `json:"price"`
	Payer                 string     `json:"payer"`
	Paid                  bool       `json:"paid"`
	OriginAgencyName      string     `json:"origin_agency_name"`
	DestinationAgencyName string     `json:"destination_agency_name"`
	CurrentAgencyName     string     `json:"current_agency_name,omitempty"`
	DepartureID           *uuid.UUID `json:"departure_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
}

type ParcelEvent struct {
	Type      string    `json:"type"`
	Label     string    `json:"label"`
	Status    string    `json:"status_after"`
	Agency    string    `json:"agency,omitempty"`
	City      string    `json:"city,omitempty"`
	Staff     string    `json:"staff,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ParcelDetail struct {
	ParcelSummary
	SenderEmail         *string       `json:"sender_email,omitempty"`
	RecipientEmail      *string       `json:"recipient_email,omitempty"`
	Description         string        `json:"description"`
	DeclaredValue       int64         `json:"declared_value"`
	Currency            string        `json:"currency"`
	OriginAgencyID      uuid.UUID     `json:"origin_agency_id"`
	OriginCity          string        `json:"origin_city"`
	DestinationAgencyID uuid.UUID     `json:"destination_agency_id"`
	DestinationCity     string        `json:"destination_city"`
	CurrentAgencyID     *uuid.UUID    `json:"current_agency_id,omitempty"`
	DepartureAt         *time.Time    `json:"departure_at,omitempty"`
	DepartureRoute      string        `json:"departure_route,omitempty"`
	Photo               *string       `json:"photo,omitempty"`
	PickupCodeSentAt    *time.Time    `json:"pickup_code_sent_at,omitempty"`
	DeliveredAt         *time.Time    `json:"delivered_at,omitempty"`
	DeliveredToName     *string       `json:"delivered_to_name,omitempty"`
	DeliveryProof       *string       `json:"delivery_proof,omitempty"`
	Signature           *string       `json:"signature,omitempty"`
	CompanyName         string        `json:"company_name"`
	Events              []ParcelEvent `json:"events"`
}

type ParcelDepartureOption struct {
	ID           uuid.UUID `json:"id"`
	DepartureAt  time.Time `json:"departure_at"`
	Status       string    `json:"status"`
	RouteName    string    `json:"route_name"`
	ToAgencyName string    `json:"to_agency_name"`
	IsDirect     bool      `json:"is_direct" doc:"Va directement à l'agence de retrait"`
	ParcelsCount int32     `json:"parcels_count"`
}

type ManifestParcel struct {
	ID                    uuid.UUID `json:"id"`
	TrackingNumber        string    `json:"tracking_number"`
	Status                string    `json:"status"`
	RecipientName         string    `json:"recipient_name"`
	Category              string    `json:"category"`
	WeightKg              float64   `json:"weight_kg"`
	Pieces                int32     `json:"pieces"`
	Description           string    `json:"description"`
	DestinationAgencyName string    `json:"destination_agency_name"`
	IsFinalLeg            bool      `json:"is_final_leg" doc:"Faux : colis en correspondance, à décharger pour un autre départ"`
}

var eventLabels = map[string]string{
	"accepted":    "Colis déposé et vérifié",
	"assigned":    "Affecté à un départ",
	"unassigned":  "Retiré du départ",
	"left_behind": "Non chargé au départ du bus",
	"loaded":      "Chargé dans le bus",
	"departed":    "Bus en route",
	"arrived":     "Arrivé en agence",
	"notified":    "Destinataire prévenu",
	"delivered":   "Remis au destinataire",
	"returned":    "Rendu à l'expéditeur",
	"cancelled":   "Envoi annulé",
	"incident":    "Réclamation ouverte",
}

func (s *Service) detail(ctx context.Context, q *store.Queries, id uuid.UUID) (ParcelDetail, error) {
	v, err := q.GetParcelView(ctx, id)
	if err != nil {
		return ParcelDetail{}, err
	}
	d := ParcelDetail{
		ParcelSummary: ParcelSummary{ID: v.ID, TrackingNumber: v.TrackingNumber, Status: v.Status, SenderName: v.SenderName,
			SenderPhone: v.SenderPhone, RecipientName: v.RecipientName, RecipientPhone: v.RecipientPhone, Category: v.Category,
			WeightKg: v.WeightKg, Pieces: v.Pieces, Price: v.Price, Payer: v.Payer, Paid: v.PaidAt != nil,
			OriginAgencyName: v.OriginAgencyName, DestinationAgencyName: v.DestinationAgencyName, CurrentAgencyName: deref(v.CurrentAgencyName),
			DepartureID: v.DepartureID, CreatedAt: v.CreatedAt},
		SenderEmail: v.SenderEmail, RecipientEmail: v.RecipientEmail, Description: v.Description, DeclaredValue: v.DeclaredValue,
		Currency: v.Currency, OriginAgencyID: v.OriginAgencyID, OriginCity: v.OriginCity, DestinationAgencyID: v.DestinationAgencyID,
		DestinationCity: v.DestinationCity, CurrentAgencyID: v.CurrentAgencyID, DepartureAt: v.DepartureAt,
		DepartureRoute: deref(v.DepartureRouteName), Photo: v.Photo, PickupCodeSentAt: v.PickupCodeSentAt,
		DeliveredAt: v.DeliveredAt, DeliveredToName: v.DeliveredToName, DeliveryProof: v.DeliveryProof, Signature: v.Signature,
		CompanyName: v.CompanyName, Events: []ParcelEvent{},
	}
	evs, err := q.ListParcelEvents(ctx, v.ID)
	for _, e := range evs {
		d.Events = append(d.Events, ParcelEvent{Type: e.EventType, Label: eventLabels[e.EventType], Status: e.StatusAfter,
			Agency: deref(e.AgencyName), City: deref(e.CityName), Staff: deref(e.StaffName), Note: e.Note, CreatedAt: e.CreatedAt})
	}
	return d, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type detailOut struct{ Body ParcelDetail }

func (s *Service) withDetail(ctx context.Context, p auth.Principal, id uuid.UUID) (*detailOut, error) {
	out := &detailOut{}
	err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		var err error
		out.Body, err = s.detail(ctx, q, id)
		if database.IsNotFound(err) {
			return apix.NotFound("Colis")
		}
		return err
	})
	return out, apix.Err(err)
}

// ---------------------------------------------------------------- Routes

func (s *Service) Register(api huma.API) {
	s.registerParcels(api)
	s.registerPublic(api)
	s.registerBaggage(api)
	s.registerIncidents(api)
}

func (s *Service) registerParcels(api huma.API) {
	huma.Register(api, apix.Op("list-parcel-tariffs", http.MethodGet, "/v1/admin/parcel-tariffs", tagParcels, "Tarifs colis", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []ParcelTariffDTO }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []ParcelTariffDTO }{Body: []ParcelTariffDTO{}}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListParcelTariffs(ctx, p.CompanyID)
				for _, t := range rows {
					out.Body = append(out.Body, ParcelTariffDTO{Category: t.Category, Label: t.Label, BasePrice: t.BasePrice, IncludedKg: t.IncludedKg,
						PricePerKg: t.PricePerKg, MaxWeightKg: t.MaxWeightKg, InsurancePercent: t.InsurancePercent})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.RequireFreshMFA(apix.Op("update-parcel-tariffs", http.MethodPut, "/v1/admin/parcel-tariffs", tagParcels,
		"Modifier les tarifs colis", apix.Staff, auth.RoleCompanyAdmin)),
		func(ctx context.Context, in *struct{ Body []ParcelTariffDTO }) (*struct{ Body []ParcelTariffDTO }, error) {
			p := auth.MustFromContext(ctx)
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				for _, t := range in.Body {
					if t.IncludedKg > t.MaxWeightKg {
						return huma.Error422UnprocessableEntity(t.Label + " : le poids inclus dépasse le poids maximal")
					}
					if _, err := q.UpsertParcelTariff(ctx, store.UpsertParcelTariffParams{CompanyID: p.CompanyID, Category: t.Category,
						Label: t.Label, BasePrice: t.BasePrice, IncludedKg: t.IncludedKg, PricePerKg: t.PricePerKg,
						MaxWeightKg: t.MaxWeightKg, InsurancePercent: t.InsurancePercent}); err != nil {
						return err
					}
				}
				return apix.Audit(ctx, q, &p.CompanyID, "parcel.tariffs_updated", "company", p.CompanyID.String(), nil, in.Body)
			})
			return &struct{ Body []ParcelTariffDTO }{Body: in.Body}, apix.Err(err)
		})

	huma.Register(api, apix.Op("quote-parcel", http.MethodPost, "/v1/admin/parcels/quote", tagParcels, "Prix d'un envoi", apix.Staff, parcelRoles...),
		func(ctx context.Context, in *struct {
			Body struct {
				Category      string  `json:"category" enum:"document,small,medium,large,fragile"`
				WeightKg      float64 `json:"weight_kg" minimum:"0.01" maximum:"500"`
				DeclaredValue int64   `json:"declared_value,omitempty" minimum:"0"`
			}
		}) (*struct{ Body ParcelQuote }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body ParcelQuote }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				t, err := q.GetParcelTariff(ctx, store.GetParcelTariffParams{CompanyID: p.CompanyID, Category: in.Body.Category})
				if err != nil {
					return huma.Error422UnprocessableEntity("Catégorie non proposée par la compagnie")
				}
				out.Body, err = price(t, in.Body.WeightKg, in.Body.DeclaredValue)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-parcel", http.MethodPost, "/v1/admin/parcels", tagParcels,
		"Enregistrer un envoi (dépôt au guichet)", apix.Staff, parcelRoles...),
		"Prix calculé par le serveur selon la catégorie, le poids et la valeur déclarée. Port payé : encaissé en espèces "+
			"(caisse de l'agence de dépôt). Port dû : encaissé à la remise. L'expéditeur et le destinataire sont prévenus par SMS."),
		func(ctx context.Context, in *struct{ Body ParcelInput }) (*struct {
			Status int
			Body   ParcelDetail
		}, error) {
			p := auth.MustFromContext(ctx)
			id, err := s.Create(ctx, p, in.Body)
			if err != nil {
				return nil, apix.Err(err)
			}
			d, err := s.withDetail(ctx, p, id)
			if err != nil {
				return nil, err
			}
			return &struct {
				Status int
				Body   ParcelDetail
			}{Status: http.StatusCreated, Body: d.Body}, nil
		})

	huma.Register(api, apix.Op("list-parcels", http.MethodGet, "/v1/admin/parcels", tagParcels, "Rechercher des colis", apix.Staff, loadRoles...),
		func(ctx context.Context, in *struct {
			apix.Page
			Status   string    `query:"status" enum:"accepted,assigned,loaded,in_transit,arrived,ready,delivered,returned,cancelled,"`
			AgencyID uuid.UUID `query:"agency_id"`
			Search   string    `query:"q" maxLength:"60" doc:"Numéro de suivi, nom ou téléphone"`
		}) (*struct{ Body []ParcelSummary }, error) {
			p := auth.MustFromContext(ctx)
			var agency *uuid.UUID
			if in.AgencyID != uuid.Nil {
				agency = &in.AgencyID
			}
			out := &struct{ Body []ParcelSummary }{Body: []ParcelSummary{}}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListParcels(ctx, store.ListParcelsParams{CompanyID: p.CompanyID, Status: apix.NilIfEmpty(in.Status),
					AgencyID: agency, Search: apix.NilIfEmpty(in.Search), Lim: in.Limit, Off: in.Offset})
				for _, r := range rows {
					out.Body = append(out.Body, ParcelSummary{ID: r.ID, TrackingNumber: r.TrackingNumber, Status: r.Status,
						SenderName: r.SenderName, SenderPhone: r.SenderPhone, RecipientName: r.RecipientName, RecipientPhone: r.RecipientPhone,
						Category: r.Category, WeightKg: r.WeightKg, Pieces: r.Pieces, Price: r.Price, Payer: r.Payer, Paid: r.PaidAt != nil,
						OriginAgencyName: r.OriginAgencyName, DestinationAgencyName: r.DestinationAgencyName,
						CurrentAgencyName: deref(r.CurrentAgencyName), DepartureID: r.DepartureID, CreatedAt: r.CreatedAt})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-parcel", http.MethodGet, "/v1/admin/parcels/{id}", tagParcels, "Détail d'un colis", apix.Staff, loadRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*detailOut, error) {
			return s.withDetail(ctx, auth.MustFromContext(ctx), in.ID)
		})

	huma.Register(api, apix.Op("find-parcel", http.MethodGet, "/v1/admin/parcels/by-tracking/{tracking}", tagParcels,
		"Retrouver un colis par son numéro (scan)", apix.Staff, loadRoles...),
		func(ctx context.Context, in *struct {
			Tracking string `path:"tracking" maxLength:"20"`
		}) (*detailOut, error) {
			p := auth.MustFromContext(ctx)
			var id uuid.UUID
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				id, err = q.GetParcelIDByTracking(ctx, in.Tracking)
				return err
			})
			if err != nil {
				return nil, apix.Err(apix.NotFound("Colis"))
			}
			return s.withDetail(ctx, p, id)
		})

	huma.Register(api, apix.Op("list-parcel-departure-options", http.MethodGet, "/v1/admin/parcels/{id}/departures", tagParcels,
		"Départs possibles pour un colis", apix.Staff, parcelRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body []ParcelDepartureOption }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []ParcelDepartureOption }{Body: []ParcelDepartureOption{}}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				v, err := q.GetParcelView(ctx, in.ID)
				if err != nil || v.CompanyID != p.CompanyID {
					return apix.NotFound("Colis")
				}
				if v.CurrentAgencyID == nil {
					return nil
				}
				rows, err := q.ListParcelDepartureOptions(ctx, store.ListParcelDepartureOptionsParams{
					CompanyID: p.CompanyID, FromAgencyID: *v.CurrentAgencyID, DestinationAgencyID: v.DestinationAgencyID})
				for _, r := range rows {
					out.Body = append(out.Body, ParcelDepartureOption{ID: r.ID, DepartureAt: r.DepartureAt, Status: r.Status, RouteName: r.RouteName,
						ToAgencyName: r.ToAgencyName, IsDirect: r.IsDirect, ParcelsCount: r.ParcelsCount})
				}
				return err
			})
			return out, apix.Err(err)
		})

	action := func(id, path, summary string, roles []string, fn func(ctx context.Context, p auth.Principal, parcelID uuid.UUID, body map[string]string) error) {
		huma.Register(api, apix.Op(id, http.MethodPost, "/v1/admin/parcels/{id}/"+path, tagParcels, summary, apix.Staff, roles...),
			func(ctx context.Context, in *struct {
				ID   uuid.UUID `path:"id"`
				Body *struct {
					DepartureID string `json:"departure_id,omitempty" format:"uuid"`
					Reason      string `json:"reason,omitempty" maxLength:"300"`
				} `required:"false"`
			}) (*detailOut, error) {
				p := auth.MustFromContext(ctx)
				body := map[string]string{}
				if in.Body != nil {
					body["departure_id"], body["reason"] = in.Body.DepartureID, in.Body.Reason
				}
				if err := fn(ctx, p, in.ID, body); err != nil {
					return nil, apix.Err(err)
				}
				return s.withDetail(ctx, p, in.ID)
			})
	}
	action("assign-parcel", "assign", "Affecter un colis à un départ", parcelRoles, func(ctx context.Context, p auth.Principal, id uuid.UUID, b map[string]string) error {
		dep, err := uuid.Parse(b["departure_id"])
		if err != nil {
			return huma.Error422UnprocessableEntity("Choisissez un départ")
		}
		return s.Assign(ctx, p, id, dep)
	})
	action("unassign-parcel", "unassign", "Retirer un colis de son départ", parcelRoles, func(ctx context.Context, p auth.Principal, id uuid.UUID, b map[string]string) error {
		return s.Unassign(ctx, p, id, b["reason"])
	})
	action("resend-parcel-code", "resend-code", "Renvoyer le code de retrait", parcelRoles, func(ctx context.Context, p auth.Principal, id uuid.UUID, _ map[string]string) error {
		return s.ResendCode(ctx, p, id)
	})
	action("cancel-parcel", "cancel", "Annuler un envoi non parti", []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager},
		func(ctx context.Context, p auth.Principal, id uuid.UUID, b map[string]string) error {
			if len(b["reason"]) < 3 {
				return huma.Error422UnprocessableEntity("Indiquez le motif")
			}
			return s.Cancel(ctx, p, id, b["reason"])
		})
	action("return-parcel", "return", "Rendre un colis à l'expéditeur", []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleParcelAgent},
		func(ctx context.Context, p auth.Principal, id uuid.UUID, b map[string]string) error {
			return s.ReturnToSender(ctx, p, id, b["reason"])
		})

	huma.Register(api, apix.WithDescription(apix.Op("deliver-parcel", http.MethodPost, "/v1/admin/parcels/{id}/deliver", tagParcels,
		"Remettre un colis au destinataire", apix.Staff, parcelRoles...),
		"Code de retrait reçu par SMS (ou, à défaut, pièce d'identité contrôlée, journalisée) et signature obligatoires. "+
			"Port dû : `collect_payment` encaisse le montant en espèces dans la caisse de l'agence."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body ParcelDeliverInput
		}) (*detailOut, error) {
			p := auth.MustFromContext(ctx)
			if err := s.Deliver(ctx, p, in.ID, in.Body); err != nil {
				return nil, apix.Err(err)
			}
			return s.withDetail(ctx, p, in.ID)
		})

	huma.Register(api, apix.WithDescription(apix.Op("scan-parcel", http.MethodPost, "/v1/admin/parcels/scan", tagParcels,
		"Scanner un colis (chargement ou réception)", apix.Staff, loadRoles...),
		"`load` : chargement dans le bus du départ indiqué (refusé si le colis est prévu sur un autre départ). "+
			"`receive` : réception à l'arrivée dans l'agence de l'agent."),
		func(ctx context.Context, in *struct {
			Body struct {
				Tracking    string     `json:"tracking" maxLength:"200" doc:"Numéro de suivi ou contenu du QR code de l'étiquette"`
				Action      string     `json:"action" enum:"load,receive"`
				DepartureID *uuid.UUID `json:"departure_id,omitempty"`
				AgencyID    *uuid.UUID `json:"agency_id,omitempty"`
			}
		}) (*detailOut, error) {
			p := auth.MustFromContext(ctx)
			tracking := TrackingFromScan(in.Body.Tracking)
			var (
				parcel store.Parcel
				err    error
			)
			if in.Body.Action == "load" {
				if in.Body.DepartureID == nil {
					return nil, huma.Error422UnprocessableEntity("Départ requis pour le chargement")
				}
				parcel, err = s.Load(ctx, p, tracking, *in.Body.DepartureID)
			} else {
				parcel, err = s.Receive(ctx, p, tracking, in.Body.AgencyID)
			}
			if err != nil {
				return nil, apix.Err(err)
			}
			return s.withDetail(ctx, p, parcel.ID)
		})

	huma.Register(api, apix.Op("list-departure-parcels", http.MethodGet, "/v1/admin/departures/{id}/parcels", tagParcels,
		"Manifeste colis d'un départ", apix.Staff, loadRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body []ManifestParcel }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []ManifestParcel }{Body: []ManifestParcel{}}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListDepartureParcels(ctx, &in.ID)
				for _, r := range rows {
					out.Body = append(out.Body, ManifestParcel{ID: r.ID, TrackingNumber: r.TrackingNumber, Status: r.Status,
						RecipientName: r.RecipientName, Category: r.Category, WeightKg: r.WeightKg, Pieces: r.Pieces,
						Description: r.Description, DestinationAgencyName: r.DestinationAgencyName, IsFinalLeg: r.IsFinalLeg})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-parcel-label", http.MethodGet, "/v1/admin/parcels/{id}/label", tagParcels,
		"Étiquette du colis (PDF avec QR code)", apix.Staff, loadRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*pdfOutput, error) {
			d, err := s.withDetail(ctx, auth.MustFromContext(ctx), in.ID)
			if err != nil {
				return nil, err
			}
			pdf, err := s.parcelLabel(d.Body)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &pdfOutput{ContentType: "application/pdf", CacheControl: "private, no-store",
				ContentDisposition: fmt.Sprintf(`inline; filename="colis-%s.pdf"`, d.Body.TrackingNumber), Body: pdf}, nil
		})
}

type pdfOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	CacheControl       string `header:"Cache-Control"`
	Body               []byte
}
