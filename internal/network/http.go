// Package network expose la compagnie, les villes et les agences
// (page publique « Agences » et administration).
package network

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

const (
	tagPublic = "Public — Agences et villes"
	tagAdmin  = "Admin — Compagnie et agences"
)

type Handler struct {
	DB *database.DB
}

// ---------------------------------------------------------------- DTO

type City struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	CountryCode string    `json:"country_code"`
}

type Company struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Slug     string    `json:"slug"`
	Phone    *string   `json:"phone,omitempty"`
	Email    *string   `json:"email,omitempty"`
	LogoURL  *string   `json:"logo_url,omitempty"`
	Currency string    `json:"currency"`
}

type CompanySettings struct {
	Company
	LegalName               *string          `json:"legal_name,omitempty"`
	Timezone                string           `json:"timezone"`
	BookingHoldMinutes      int32            `json:"booking_hold_minutes" doc:"Durée de blocage d'un siège pendant le paiement"`
	SalesCutoffMinutes      int32            `json:"sales_cutoff_minutes" doc:"Fermeture des ventes avant le départ"`
	CancellationCutoffHours int32            `json:"cancellation_cutoff_hours" doc:"Annulation client autorisée jusqu'à N heures avant le départ"`
	CancellationFeePercent  int32            `json:"cancellation_fee_percent" doc:"Frais retenus en cas d'annulation"`
	RefundApprovalThreshold int64            `json:"refund_approval_threshold" doc:"Au-delà, un remboursement exige une validation par une seconde personne"`
	ChangeEnabled           bool             `json:"change_enabled" doc:"Les clients peuvent changer de départ"`
	ChangeCutoffHours       int32            `json:"change_cutoff_hours" doc:"Modification autorisée jusqu'à N heures avant le départ"`
	ChangeFeeAmount         int64            `json:"change_fee_amount" doc:"Frais de modification par passager"`
	ChangeMaxCount          int32            `json:"change_max_count" doc:"Nombre maximal de modifications par réservation"`
	ChangeWindowDays        int32            `json:"change_window_days" doc:"Le nouveau départ doit avoir lieu dans les N jours suivant le départ initial"`
	BaggageExtraPerKg       int64            `json:"baggage_extra_per_kg" doc:"Excédent de bagage : prix par kilo au-delà du poids inclus"`
	TaxID                   *string          `json:"tax_id,omitempty" doc:"IFU, imprimé sur les factures"`
	TradeRegister           *string          `json:"trade_register,omitempty" doc:"RCCM"`
	Address                 *string          `json:"address,omitempty" doc:"Adresse du siège"`
	InvoicePrefix           string           `json:"invoice_prefix" doc:"Préfixe des numéros de facture (ex. FAC → FAC-2026-000001)"`
	InvoiceFooter           string           `json:"invoice_footer" doc:"Mentions en pied de facture (régime fiscal, conditions…)"`
	PayLater                PayLaterSettings `json:"pay_later" doc:"Réservation sans paiement immédiat (payer en agence)"`
	OnboardQuotaSeats       int32            `json:"onboard_quota_seats" doc:"Places réservées au contrôleur sur chaque nouveau départ (vente à bord sans réseau)"`
}

// PayLaterSettings : règles du paiement en agence.
type PayLaterSettings struct {
	Enabled       bool  `json:"enabled"`
	HoldHours     int32 `json:"hold_hours" minimum:"1" maximum:"168" default:"24" doc:"Délai accordé pour payer"`
	CutoffMinutes int32 `json:"cutoff_minutes" minimum:"0" maximum:"1440" default:"120" doc:"Paiement exigé au plus tard N minutes avant le départ"`
	MaxSeats      int32 `json:"max_seats" minimum:"1" maximum:"10" default:"4" doc:"Places au plus par réservation"`
	MaxActive     int32 `json:"max_active" minimum:"1" maximum:"10" default:"2" doc:"Réservations en attente au plus par numéro de téléphone"`
	QuotaPercent  int32 `json:"quota_percent" minimum:"1" maximum:"100" default:"30" doc:"Part maximale des sièges d'un départ en attente de paiement"`
	Strikes       int32 `json:"strikes" minimum:"1" maximum:"20" default:"3" doc:"Réservations non payées (30 jours) avant retrait de l'option pour 30 jours"`
}

type OpeningHours struct {
	Day   string `json:"day" enum:"mon,tue,wed,thu,fri,sat,sun"`
	Open  string `json:"open" pattern:"^[0-2][0-9]:[0-5][0-9]$" example:"06:00"`
	Close string `json:"close" pattern:"^[0-2][0-9]:[0-5][0-9]$" example:"20:00"`
}

type Agency struct {
	ID           uuid.UUID      `json:"id"`
	CompanyID    uuid.UUID      `json:"company_id"`
	CompanyName  string         `json:"company_name,omitempty"`
	CityID       uuid.UUID      `json:"city_id"`
	CityName     string         `json:"city_name"`
	CitySlug     string         `json:"city_slug,omitempty"`
	Name         string         `json:"name"`
	Slug         string         `json:"slug"`
	Address      string         `json:"address"`
	Landmark     string         `json:"landmark" doc:"Point de repère"`
	Latitude     *float64       `json:"latitude,omitempty"`
	Longitude    *float64       `json:"longitude,omitempty"`
	Phone        string         `json:"phone"`
	WhatsApp     string         `json:"whatsapp"`
	Email        string         `json:"email"`
	OpeningHours []OpeningHours `json:"opening_hours"`
	Services     []string       `json:"services" doc:"ticketing, parcel_drop, parcel_pickup, lounge"`
	Instructions string         `json:"instructions" doc:"Consignes aux voyageurs"`
	Photos       []string       `json:"photos"`
	IsActive     bool           `json:"is_active"`
}

type UpcomingDeparture struct {
	ID                    uuid.UUID `json:"id"`
	DepartureAt           time.Time `json:"departure_at"`
	ArrivalEta            time.Time `json:"arrival_eta"`
	BasePrice             int64     `json:"base_price"`
	Status                string    `json:"status"`
	DestinationCity       string    `json:"destination_city"`
	DestinationAgencyName string    `json:"destination_agency_name"`
	AvailableSeats        int32     `json:"available_seats"`
}

type AgencyDetail struct {
	Agency
	Destinations       []City              `json:"destinations"`
	UpcomingDepartures []UpcomingDeparture `json:"upcoming_departures" doc:"Départs des 3 prochains jours"`
}

func toAgency(a store.Agency, cityName, citySlug, companyName string) Agency {
	out := Agency{
		ID: a.ID, CompanyID: a.CompanyID, CompanyName: companyName, CityID: a.CityID, CityName: cityName, CitySlug: citySlug,
		Name: a.Name, Slug: a.Slug, Address: a.Address, Landmark: a.Landmark, Latitude: a.Latitude, Longitude: a.Longitude,
		Phone: a.Phone, WhatsApp: a.Whatsapp, Email: a.Email, Services: a.Services, Instructions: a.Instructions,
		Photos: a.Photos, IsActive: a.IsActive, OpeningHours: []OpeningHours{},
	}
	_ = json.Unmarshal(a.OpeningHours, &out.OpeningHours)
	return out
}

func toCompany(c store.Company) Company {
	return Company{ID: c.ID, Name: c.Name, Slug: c.Slug, Phone: c.Phone, Email: c.Email, LogoURL: c.LogoUrl, Currency: c.Currency}
}

func toSettings(c store.Company) CompanySettings {
	return CompanySettings{
		Company: toCompany(c), LegalName: c.LegalName, Timezone: c.Timezone,
		BookingHoldMinutes: c.BookingHoldMinutes, SalesCutoffMinutes: c.SalesCutoffMinutes,
		CancellationCutoffHours: c.CancellationCutoffHours, CancellationFeePercent: c.CancellationFeePercent,
		RefundApprovalThreshold: c.RefundApprovalThreshold,
		ChangeEnabled:           c.ChangeEnabled, ChangeCutoffHours: c.ChangeCutoffHours, ChangeFeeAmount: c.ChangeFeeAmount,
		ChangeMaxCount: c.ChangeMaxCount, ChangeWindowDays: c.ChangeWindowDays, BaggageExtraPerKg: c.BaggageExtraPerKg,
		TaxID: c.TaxID, TradeRegister: c.TradeRegister, Address: c.Address, InvoicePrefix: c.InvoicePrefix, InvoiceFooter: c.InvoiceFooter,
		PayLater: PayLaterSettings{Enabled: c.PayLaterEnabled, HoldHours: c.PayLaterHoldHours, CutoffMinutes: c.PayLaterCutoffMinutes,
			MaxSeats: c.PayLaterMaxSeats, MaxActive: c.PayLaterMaxActive, QuotaPercent: c.PayLaterQuotaPercent, Strikes: c.PayLaterStrikes},
		OnboardQuotaSeats: c.OnboardQuotaSeats,
	}
}

// Slugify : "Agence de Cotonou – Jonquet" → "agence-de-cotonou-jonquet".
func Slugify(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	s, _, _ = transform.String(t, strings.ToLower(s))
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// ---------------------------------------------------------------- Routes publiques

func (h *Handler) Register(api huma.API) {
	huma.Register(api, apix.Op("list-cities", http.MethodGet, "/v1/public/cities", tagPublic, "Lister les villes", apix.Public),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []City }, error) {
			out := &struct{ Body []City }{Body: []City{}}
			err := h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCities(ctx)
				for _, c := range rows {
					out.Body = append(out.Body, City{ID: c.ID, Name: c.Name, Slug: c.Slug, CountryCode: c.CountryCode})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-companies", http.MethodGet, "/v1/public/companies", tagPublic, "Lister les compagnies", apix.Public),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Company }, error) {
			out := &struct{ Body []Company }{Body: []Company{}}
			err := h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListActiveCompanies(ctx)
				for _, c := range rows {
					out.Body = append(out.Body, toCompany(c))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("list-public-agencies", http.MethodGet, "/v1/public/agencies", tagPublic,
		"Lister les agences (page Agences)", apix.Public), "Filtres combinables : ville, service, compagnie, recherche libre."),
		func(ctx context.Context, in *struct {
			City      string    `query:"city" doc:"Slug de la ville" example:"cotonou"`
			Service   string    `query:"service" enum:"ticketing,parcel_drop,parcel_pickup,lounge"`
			CompanyID uuid.UUID `query:"company_id"`
			Q         string    `query:"q" maxLength:"80" doc:"Recherche par nom, ville ou adresse"`
		}) (*struct{ Body []Agency }, error) {
			params := store.ListPublicAgenciesParams{CitySlug: apix.NilIfEmpty(in.City), Service: apix.NilIfEmpty(in.Service), Q: apix.NilIfEmpty(in.Q)}
			if in.CompanyID != uuid.Nil {
				params.CompanyID = &in.CompanyID
			}
			out := &struct{ Body []Agency }{Body: []Agency{}}
			err := h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListPublicAgencies(ctx, params)
				for _, r := range rows {
					out.Body = append(out.Body, toAgency(r.Agency, r.CityName, r.CitySlug, r.CompanyName))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-public-agency", http.MethodGet, "/v1/public/agencies/{slug}", tagPublic,
		"Fiche d'une agence (contacts, horaires, services, départs à venir)", apix.Public),
		func(ctx context.Context, in *struct {
			Slug string `path:"slug" maxLength:"120"`
		}) (*struct{ Body AgencyDetail }, error) {
			out := &struct{ Body AgencyDetail }{}
			err := h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				r, err := q.GetPublicAgencyBySlug(ctx, in.Slug)
				if database.IsNotFound(err) {
					return apix.NotFound("Agence")
				}
				if err != nil {
					return err
				}
				out.Body.Agency = toAgency(r.Agency, r.CityName, r.CitySlug, r.CompanyName)
				dests, err := q.ListDestinationsFromAgency(ctx, &r.Agency.ID)
				if err != nil {
					return err
				}
				out.Body.Destinations = []City{}
				for _, d := range dests {
					out.Body.Destinations = append(out.Body.Destinations, City{Name: d.CityName, Slug: d.CitySlug})
				}
				deps, err := q.ListUpcomingDeparturesFromAgency(ctx, &r.Agency.ID)
				if err != nil {
					return err
				}
				out.Body.UpcomingDepartures = []UpcomingDeparture{}
				for _, d := range deps {
					out.Body.UpcomingDepartures = append(out.Body.UpcomingDepartures, UpcomingDeparture(d))
				}
				return nil
			})
			return out, apix.Err(err)
		})

	h.registerShowcase(api)
	h.registerAdmin(api)
}

// ---------------------------------------------------------------- Routes d'administration

type AgencyInput struct {
	CityID       uuid.UUID      `json:"city_id"`
	Name         string         `json:"name" minLength:"2" maxLength:"120"`
	Address      string         `json:"address" maxLength:"250"`
	Landmark     string         `json:"landmark,omitempty" maxLength:"250"`
	Latitude     *float64       `json:"latitude,omitempty" minimum:"-90" maximum:"90"`
	Longitude    *float64       `json:"longitude,omitempty" minimum:"-180" maximum:"180"`
	Phone        string         `json:"phone,omitempty" maxLength:"20"`
	WhatsApp     string         `json:"whatsapp,omitempty" maxLength:"20"`
	Email        string         `json:"email,omitempty" maxLength:"254"`
	OpeningHours []OpeningHours `json:"opening_hours,omitempty" maxItems:"14"`
	Services     []string       `json:"services" minItems:"1" uniqueItems:"true" enum:"ticketing,parcel_drop,parcel_pickup,lounge"`
	Instructions string         `json:"instructions,omitempty" maxLength:"2000"`
	Photos       []string       `json:"photos,omitempty" maxItems:"10"`
	IsActive     bool           `json:"is_active"`
}

func (in AgencyInput) hours() json.RawMessage {
	if in.OpeningHours == nil {
		in.OpeningHours = []OpeningHours{}
	}
	b, _ := json.Marshal(in.OpeningHours)
	return b
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (h *Handler) registerAdmin(api huma.API) {
	admin := []string{auth.RoleCompanyAdmin}
	h.registerStopPoints(api, admin)

	huma.Register(api, apix.Op("get-company-settings", http.MethodGet, "/v1/admin/company", tagAdmin, "Paramètres de la compagnie", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body CompanySettings }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body CompanySettings }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				c, err := q.GetCompany(ctx, p.CompanyID)
				out.Body = toSettings(c)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.RequireFreshMFA(apix.Op("update-company-settings", http.MethodPut, "/v1/admin/company", tagAdmin, "Modifier les paramètres de la compagnie", apix.Staff, admin...)),
		func(ctx context.Context, in *struct {
			Body struct {
				Name                    string            `json:"name" minLength:"2" maxLength:"120"`
				LegalName               *string           `json:"legal_name,omitempty" maxLength:"200"`
				Phone                   *string           `json:"phone,omitempty" maxLength:"20"`
				Email                   *string           `json:"email,omitempty" maxLength:"254"`
				LogoURL                 *string           `json:"logo_url,omitempty" maxLength:"500"`
				BookingHoldMinutes      int32             `json:"booking_hold_minutes" minimum:"2" maximum:"60"`
				SalesCutoffMinutes      int32             `json:"sales_cutoff_minutes" minimum:"0" maximum:"720"`
				CancellationCutoffHours int32             `json:"cancellation_cutoff_hours" minimum:"0" maximum:"168"`
				CancellationFeePercent  int32             `json:"cancellation_fee_percent" minimum:"0" maximum:"100"`
				RefundApprovalThreshold int64             `json:"refund_approval_threshold" minimum:"0"`
				ChangeEnabled           bool              `json:"change_enabled"`
				ChangeCutoffHours       int32             `json:"change_cutoff_hours" minimum:"0" maximum:"168"`
				ChangeFeeAmount         int64             `json:"change_fee_amount" minimum:"0" maximum:"1000000"`
				ChangeMaxCount          int32             `json:"change_max_count" minimum:"0" maximum:"5"`
				ChangeWindowDays        int32             `json:"change_window_days" minimum:"1" maximum:"365"`
				BaggageExtraPerKg       int64             `json:"baggage_extra_per_kg" minimum:"0" maximum:"100000"`
				TaxID                   *string           `json:"tax_id,omitempty" maxLength:"30"`
				TradeRegister           *string           `json:"trade_register,omitempty" maxLength:"60"`
				Address                 *string           `json:"address,omitempty" maxLength:"300"`
				InvoicePrefix           string            `json:"invoice_prefix,omitempty" pattern:"^[A-Z0-9]{2,8}$" default:"FAC"`
				InvoiceFooter           string            `json:"invoice_footer,omitempty" maxLength:"1000"`
				PayLater                *PayLaterSettings `json:"pay_later,omitempty" doc:"Absent : règles inchangées"`
				OnboardQuotaSeats       *int32            `json:"onboard_quota_seats,omitempty" minimum:"0" maximum:"20" doc:"Absent : inchangé"`
			}
		}) (*struct{ Body CompanySettings }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body CompanySettings }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetCompany(ctx, p.CompanyID)
				if err != nil {
					return err
				}
				pl := toSettings(before).PayLater
				if b.PayLater != nil {
					pl = *b.PayLater
				}
				c, err := q.UpdateCompanySettings(ctx, store.UpdateCompanySettingsParams{
					PayLaterEnabled: pl.Enabled, PayLaterHoldHours: pl.HoldHours, PayLaterCutoffMinutes: pl.CutoffMinutes,
					PayLaterMaxSeats: pl.MaxSeats, PayLaterMaxActive: pl.MaxActive, PayLaterQuotaPercent: pl.QuotaPercent,
					PayLaterStrikes: pl.Strikes,
					ID:              p.CompanyID, Name: b.Name, LegalName: b.LegalName, Phone: b.Phone, Email: b.Email, LogoUrl: b.LogoURL,
					BookingHoldMinutes: b.BookingHoldMinutes, SalesCutoffMinutes: b.SalesCutoffMinutes,
					CancellationCutoffHours: b.CancellationCutoffHours, CancellationFeePercent: b.CancellationFeePercent,
					RefundApprovalThreshold: b.RefundApprovalThreshold,
					ChangeEnabled:           b.ChangeEnabled, ChangeCutoffHours: b.ChangeCutoffHours, ChangeFeeAmount: b.ChangeFeeAmount,
					ChangeMaxCount: b.ChangeMaxCount, ChangeWindowDays: b.ChangeWindowDays, BaggageExtraPerKg: b.BaggageExtraPerKg,
					TaxID: b.TaxID, TradeRegister: b.TradeRegister, Address: b.Address, InvoicePrefix: prefixOr(b.InvoicePrefix), InvoiceFooter: b.InvoiceFooter,
					OnboardQuotaSeats: b.OnboardQuotaSeats,
				})
				if err != nil {
					return err
				}
				out.Body = toSettings(c)
				return apix.Audit(ctx, q, &p.CompanyID, "company.settings_updated", "company", c.ID.String(), toSettings(before), out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("create-city", http.MethodPost, "/v1/admin/cities", tagAdmin, "Ajouter une ville", apix.Staff, admin...),
		func(ctx context.Context, in *struct {
			Body struct {
				Name        string `json:"name" minLength:"2" maxLength:"80"`
				CountryCode string `json:"country_code,omitempty" pattern:"^[A-Z]{2}$" default:"BJ"`
			}
		}) (*struct{ Body City }, error) {
			out := &struct{ Body City }{}
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				c, err := q.CreateCity(ctx, store.CreateCityParams{Name: in.Body.Name, Slug: Slugify(in.Body.Name), CountryCode: in.Body.CountryCode})
				out.Body = City{ID: c.ID, Name: c.Name, Slug: c.Slug, CountryCode: c.CountryCode}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-agencies", http.MethodGet, "/v1/admin/agencies", tagAdmin, "Lister les agences de la compagnie", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Agency }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Agency }{Body: []Agency{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListAgencies(ctx, p.CompanyID)
				for _, r := range rows {
					out.Body = append(out.Body, toAgency(r.Agency, r.CityName, "", ""))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("create-agency", http.MethodPost, "/v1/admin/agencies", tagAdmin, "Créer une agence", apix.Staff, admin...),
		func(ctx context.Context, in *struct{ Body AgencyInput }) (*struct{ Body Agency }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body Agency }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				company, err := q.GetCompany(ctx, p.CompanyID)
				if err != nil {
					return err
				}
				city, err := q.GetCity(ctx, b.CityID)
				if database.IsNotFound(err) {
					return huma.Error422UnprocessableEntity("Ville inconnue")
				}
				if err != nil {
					return err
				}
				a, err := q.CreateAgency(ctx, store.CreateAgencyParams{
					CompanyID: p.CompanyID, CityID: b.CityID, Name: b.Name, Slug: Slugify(company.Slug + " " + b.Name),
					Address: b.Address, Landmark: b.Landmark, Latitude: b.Latitude, Longitude: b.Longitude,
					Phone: b.Phone, Whatsapp: b.WhatsApp, Email: b.Email, OpeningHours: b.hours(),
					Services: b.Services, Instructions: b.Instructions, Photos: emptyIfNil(b.Photos), IsActive: b.IsActive,
				})
				if err != nil {
					return err
				}
				// Une agence est aussi un point d'arrêt où l'on monte.
				if err := EnsureAgencyStopPoint(ctx, q, a); err != nil {
					return err
				}
				out.Body = toAgency(a, city.Name, city.Slug, company.Name)
				return apix.Audit(ctx, q, &p.CompanyID, "agency.created", "agency", a.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("update-agency", http.MethodPut, "/v1/admin/agencies/{id}", tagAdmin, "Modifier une agence", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body AgencyInput
		}) (*struct{ Body Agency }, error) {
			p := auth.MustFromContext(ctx)
			// Un chef d'agence ne peut modifier que sa propre agence.
			if p.Role == auth.RoleAgencyManager && (p.AgencyID == nil || *p.AgencyID != in.ID) {
				return nil, huma.Error403Forbidden("Vous ne pouvez modifier que votre agence")
			}
			b := in.Body
			out := &struct{ Body Agency }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetAgency(ctx, store.GetAgencyParams{ID: in.ID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return apix.NotFound("Agence")
				}
				if err != nil {
					return err
				}
				city, err := q.GetCity(ctx, b.CityID)
				if err != nil {
					return huma.Error422UnprocessableEntity("Ville inconnue")
				}
				a, err := q.UpdateAgency(ctx, store.UpdateAgencyParams{
					ID: in.ID, CompanyID: p.CompanyID, CityID: b.CityID, Name: b.Name,
					Address: b.Address, Landmark: b.Landmark, Latitude: b.Latitude, Longitude: b.Longitude,
					Phone: b.Phone, Whatsapp: b.WhatsApp, Email: b.Email, OpeningHours: b.hours(),
					Services: b.Services, Instructions: b.Instructions, Photos: emptyIfNil(b.Photos), IsActive: b.IsActive,
				})
				if err != nil {
					return err
				}
				out.Body = toAgency(a, city.Name, city.Slug, "")
				return apix.Audit(ctx, q, &p.CompanyID, "agency.updated", "agency", a.ID.String(), toAgency(before, "", "", ""), out.Body)
			})
			return out, apix.Err(err)
		})
}

func prefixOr(p string) string {
	if p == "" {
		return "FAC"
	}
	return p
}
