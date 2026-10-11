// Package review gère les avis des voyageurs : invitation après le voyage
// (lien personnel, sans connexion), dépôt de l'avis, modération et réponse
// de la compagnie, affichage des avis publiés sur le site.
//
// Une note globale de 2 ou moins ouvre automatiquement une réclamation.
package review

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/refgen"
	"busvoyage/internal/store"
)

const (
	tag = "Avis"
	// InvitationValidity : délai pour donner son avis après l'invitation.
	InvitationValidity = 14 * 24 * time.Hour
	// incidentThreshold : note globale qui déclenche une réclamation.
	incidentThreshold = 2
)

var moderators = []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager}

// NewToken : jeton d'invitation (envoyé au client) et son empreinte (stockée).
func NewToken() (token string, hash []byte) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

type Handler struct {
	DB *database.DB
}

// ---------------------------------------------------------------- DTO

type Invitation struct {
	CompanyName string    `json:"company_name"`
	Origin      string    `json:"origin_city"`
	Destination string    `json:"destination_city"`
	DepartureAt time.Time `json:"departure_at"`
	Reference   string    `json:"reference"`
	FirstName   string    `json:"first_name"`
	Reviewed    bool      `json:"reviewed" doc:"Avis déjà donné"`
	Expired     bool      `json:"expired"`
}

type ReviewInput struct {
	Rating         int16  `json:"rating" minimum:"1" maximum:"5"`
	Punctuality    *int16 `json:"punctuality,omitempty" minimum:"1" maximum:"5"`
	Comfort        *int16 `json:"comfort,omitempty" minimum:"1" maximum:"5"`
	Staff          *int16 `json:"staff,omitempty" minimum:"1" maximum:"5"`
	Cleanliness    *int16 `json:"cleanliness,omitempty" minimum:"1" maximum:"5"`
	Comment        string `json:"comment,omitempty" maxLength:"1000"`
	PublishConsent bool   `json:"publish_consent" doc:"Accord pour publier l'avis avec le prénom et la ville"`
}

type PublicReview struct {
	ID          uuid.UUID `json:"id"`
	Rating      int16     `json:"rating"`
	Comment     string    `json:"comment"`
	Name        string    `json:"name" doc:"Prénom"`
	City        string    `json:"city"`
	Route       string    `json:"route"`
	CompanyName string    `json:"company_name"`
	Reply       string    `json:"reply,omitempty" doc:"Réponse de la compagnie"`
	CreatedAt   time.Time `json:"created_at"`
}

type PublicReviews struct {
	Average float64        `json:"average"`
	Total   int32          `json:"total"`
	Reviews []PublicReview `json:"reviews"`
}

type Review struct {
	ID                uuid.UUID  `json:"id"`
	Reference         string     `json:"reference"`
	BookingID         uuid.UUID  `json:"booking_id"`
	RouteName         string     `json:"route_name"`
	DepartureAt       time.Time  `json:"departure_at"`
	Rating            int16      `json:"rating"`
	Punctuality       *int16     `json:"punctuality,omitempty"`
	Comfort           *int16     `json:"comfort,omitempty"`
	Staff             *int16     `json:"staff,omitempty"`
	Cleanliness       *int16     `json:"cleanliness,omitempty"`
	Comment           string     `json:"comment"`
	Name              string     `json:"name"`
	City              string     `json:"city"`
	PublishConsent    bool       `json:"publish_consent"`
	Status            string     `json:"status" enum:"pending,published,hidden"`
	HiddenReason      string     `json:"hidden_reason,omitempty"`
	Reply             string     `json:"reply,omitempty"`
	RepliedAt         *time.Time `json:"replied_at,omitempty"`
	IncidentID        *uuid.UUID `json:"incident_id,omitempty"`
	IncidentReference string     `json:"incident_reference,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

type ReviewStats struct {
	Total       int32   `json:"total"`
	Average     float64 `json:"average"`
	Positive    int32   `json:"positive" doc:"Notes de 4 ou 5"`
	Negative    int32   `json:"negative" doc:"Notes de 1 ou 2"`
	ToModerate  int32   `json:"to_moderate"`
	Replied     int32   `json:"replied"`
	Punctuality float64 `json:"punctuality"`
	Comfort     float64 `json:"comfort"`
	Staff       float64 `json:"staff"`
	Cleanliness float64 `json:"cleanliness"`
}

type ReviewList struct {
	ReviewStats ReviewStats `json:"stats" doc:"30 derniers jours"`
	Reviews     []Review    `json:"reviews"`
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return "Voyageur"
}

// ---------------------------------------------------------------- Routes

func (h *Handler) Register(api huma.API) {
	h.registerPublic(api)
	h.registerAdmin(api)
}

func (h *Handler) invitation(ctx context.Context, q *store.Queries, token string) (store.GetReviewInvitationRow, error) {
	inv, err := q.GetReviewInvitation(ctx, HashToken(token))
	if err != nil {
		return inv, apix.NotFound("Invitation")
	}
	return inv, nil
}

func (h *Handler) registerPublic(api huma.API) {
	type tokenPath struct {
		Token string `path:"token" minLength:"20" maxLength:"64"`
	}

	huma.Register(api, apix.Op("get-review-invitation", http.MethodGet, "/v1/public/reviews/{token}", tag,
		"Invitation à donner son avis (lien reçu après le voyage)", apix.Public),
		func(ctx context.Context, in *tokenPath) (*struct{ Body Invitation }, error) {
			out := &struct{ Body Invitation }{}
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				inv, err := h.invitation(ctx, q, in.Token)
				if err != nil {
					return err
				}
				out.Body = Invitation{CompanyName: inv.CompanyName, Origin: inv.OriginCity, Destination: inv.DestinationCity,
					DepartureAt: inv.DepartureAt, Reference: inv.Reference, FirstName: firstName(inv.ContactName),
					Reviewed: inv.Reviewed, Expired: time.Now().After(inv.ExpiresAt)}
				return nil
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("submit-review", http.MethodPost, "/v1/public/reviews/{token}", tag,
		"Donner son avis", apix.Public),
		"Un avis par réservation. Une note globale de 2 ou moins ouvre une réclamation traitée par le service client. "+
			"L'avis n'est publié sur le site qu'avec l'accord du voyageur et après modération."),
		func(ctx context.Context, in *struct {
			Token string `path:"token" minLength:"20" maxLength:"64"`
			Body  ReviewInput
		}) (*struct{}, error) {
			b := in.Body
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				inv, err := h.invitation(ctx, q, in.Token)
				if err != nil {
					return err
				}
				if inv.Reviewed {
					return huma.Error409Conflict("Merci, vous avez déjà donné votre avis pour ce voyage")
				}
				if time.Now().After(inv.ExpiresAt) {
					return huma.Error410Gone("Ce lien a expiré")
				}
				city := inv.OriginCity
				rv, err := q.InsertReview(ctx, store.InsertReviewParams{
					CompanyID: inv.CompanyID, BookingID: inv.BookingID, DepartureID: inv.DepartureID, RouteID: inv.RouteID,
					AgencyID: inv.OriginAgencyID, CustomerID: inv.CustomerID, Rating: b.Rating, Punctuality: b.Punctuality,
					Comfort: b.Comfort, Staff: b.Staff, Cleanliness: b.Cleanliness, Comment: strings.TrimSpace(b.Comment),
					DisplayName: firstName(inv.ContactName), City: city, PublishConsent: b.PublishConsent,
				})
				if database.UniqueViolation(err) != "" {
					return huma.Error409Conflict("Merci, vous avez déjà donné votre avis pour ce voyage")
				}
				if err != nil {
					return err
				}
				if err := q.MarkReviewInvitationUsed(ctx, inv.ID); err != nil {
					return err
				}
				if b.Rating <= incidentThreshold {
					return openIncident(ctx, q, inv, rv)
				}
				return nil
			})
			return nil, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-public-reviews", http.MethodGet, "/v1/public/reviews", tag,
		"Avis publiés (site public)", apix.Public),
		func(ctx context.Context, in *struct {
			Limit    int32     `query:"limit" minimum:"1" maximum:"30" default:"6"`
			RouteID  uuid.UUID `query:"route_id"`
			AgencyID uuid.UUID `query:"agency_id"`
		}) (*struct {
			CacheControl string `header:"Cache-Control"`
			Body         PublicReviews
		}, error) {
			var route, agency *uuid.UUID
			if in.RouteID != uuid.Nil {
				route = &in.RouteID
			}
			if in.AgencyID != uuid.Nil {
				agency = &in.AgencyID
			}
			out := &struct {
				CacheControl string `header:"Cache-Control"`
				Body         PublicReviews
			}{CacheControl: "public, max-age=300", Body: PublicReviews{Reviews: []PublicReview{}}}
			err := h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				sum, err := q.PublishedReviewSummary(ctx, store.PublishedReviewSummaryParams{RouteID: route, AgencyID: agency})
				if err != nil {
					return err
				}
				out.Body.Average, out.Body.Total = sum.Average, sum.Total
				rows, err := q.ListPublishedReviews(ctx, store.ListPublishedReviewsParams{RouteID: route, AgencyID: agency, Lim: in.Limit})
				for _, r := range rows {
					out.Body.Reviews = append(out.Body.Reviews, PublicReview{ID: r.ID, Rating: r.Rating, Comment: r.Comment,
						Name: r.DisplayName, City: r.City, Route: r.OriginCity + " → " + r.DestinationCity,
						CompanyName: r.CompanyName, Reply: r.Reply, CreatedAt: r.CreatedAt})
				}
				return err
			})
			return out, apix.Err(err)
		})
}

// openIncident : une mauvaise note devient une réclamation suivie par le service client.
func openIncident(ctx context.Context, q *store.Queries, inv store.GetReviewInvitationRow, rv store.Review) error {
	desc := fmt.Sprintf("Avis client : note %d/5 pour le voyage %s → %s du %s.", rv.Rating, inv.OriginCity, inv.DestinationCity,
		inv.DepartureAt.In(time.FixedZone("WAT", 3600)).Format("02/01/2006 15h04"))
	if rv.Comment != "" {
		desc += "\n\n« " + rv.Comment + " »"
	}
	b, err := q.GetBooking(ctx, inv.BookingID)
	if err != nil {
		return err
	}
	i, err := q.InsertIncident(ctx, store.InsertIncidentParams{
		CompanyID: inv.CompanyID, Reference: refgen.Incident(), SubjectType: "booking", BookingID: &inv.BookingID,
		Category: "other", Description: desc, ReporterType: "customer", ReporterName: b.ContactName,
		ReporterPhone: b.ContactPhone, ReporterCustomerID: b.CustomerID, Priority: "high",
	})
	if err != nil {
		return err
	}
	if err := q.InsertIncidentEvent(ctx, store.InsertIncidentEventParams{
		CompanyID: inv.CompanyID, IncidentID: i.ID, AuthorType: "system", AuthorName: "Avis voyageur",
		Action: "created", Note: "Réclamation ouverte automatiquement après un avis négatif.", IsPublic: false,
	}); err != nil {
		return err
	}
	return q.SetReviewIncident(ctx, store.SetReviewIncidentParams{ID: rv.ID, IncidentID: &i.ID})
}

func toReview(r store.ListReviewsRow) Review {
	return Review{ID: r.ID, Reference: r.Reference, BookingID: r.BookingID, RouteName: r.RouteName, DepartureAt: r.DepartureAt,
		Rating: r.Rating, Punctuality: r.Punctuality, Comfort: r.Comfort, Staff: r.Staff, Cleanliness: r.Cleanliness,
		Comment: r.Comment, Name: r.DisplayName, City: r.City, PublishConsent: r.PublishConsent, Status: r.Status,
		HiddenReason: r.HiddenReason, Reply: r.Reply, RepliedAt: r.RepliedAt, IncidentID: r.IncidentID,
		IncidentReference: deref(r.IncidentReference), CreatedAt: r.CreatedAt}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *Handler) registerAdmin(api huma.API) {
	huma.Register(api, apix.Op("list-reviews", http.MethodGet, "/v1/admin/reviews", tag, "Avis des voyageurs", apix.Staff, moderators...),
		func(ctx context.Context, in *struct {
			apix.Page
			Status    string `query:"status" enum:"pending,published,hidden,"`
			MaxRating int32  `query:"max_rating" minimum:"0" maximum:"5" doc:"Ex. 2 : avis négatifs seulement"`
		}) (*struct{ Body ReviewList }, error) {
			p := auth.MustFromContext(ctx)
			params := store.ListReviewsParams{CompanyID: p.CompanyID, Status: apix.NilIfEmpty(in.Status), Lim: in.Limit, Off: in.Offset}
			if in.MaxRating > 0 {
				params.MaxRating = &in.MaxRating
			}
			out := &struct{ Body ReviewList }{Body: ReviewList{Reviews: []Review{}}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				s, err := q.ReviewStats(ctx, store.ReviewStatsParams{CompanyID: p.CompanyID, Since: time.Now().AddDate(0, 0, -30)})
				if err != nil {
					return err
				}
				out.Body.ReviewStats = ReviewStats{Total: s.Total, Average: s.Average, Positive: s.Positive, Negative: s.Negative,
					ToModerate: s.ToModerate, Replied: s.Replied, Punctuality: s.Punctuality, Comfort: s.Comfort,
					Staff: s.Staff, Cleanliness: s.Cleanliness}
				rows, err := q.ListReviews(ctx, params)
				for _, r := range rows {
					out.Body.Reviews = append(out.Body.Reviews, toReview(r))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("moderate-review", http.MethodPut, "/v1/admin/reviews/{id}", tag,
		"Publier, masquer ou répondre à un avis", apix.Staff, moderators...),
		"La modération ne modifie jamais le texte du voyageur : elle le publie ou le masque (motif). "+
			"La réponse de la compagnie est publiée sous l'avis."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Status       string `json:"status" enum:"pending,published,hidden"`
				HiddenReason string `json:"hidden_reason,omitempty" maxLength:"200"`
				Reply        string `json:"reply,omitempty" maxLength:"1000"`
			}
		}) (*struct{}, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetReview(ctx, in.ID)
				if err != nil || before.CompanyID != p.CompanyID {
					return apix.NotFound("Avis")
				}
				if b.Status == "published" && !before.PublishConsent {
					return huma.Error422UnprocessableEntity("Le voyageur n'a pas autorisé la publication de cet avis")
				}
				if b.Status == "hidden" && strings.TrimSpace(b.HiddenReason) == "" {
					return huma.Error422UnprocessableEntity("Indiquez le motif du masquage")
				}
				staffID := p.ID
				after, err := q.ModerateReview(ctx, store.ModerateReviewParams{ID: in.ID, CompanyID: p.CompanyID, Status: b.Status,
					HiddenReason: strings.TrimSpace(b.HiddenReason), Reply: strings.TrimSpace(b.Reply), StaffID: &staffID})
				if err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "review.moderated", "review", in.ID.String(),
					map[string]any{"status": before.Status, "reply": before.Reply},
					map[string]any{"status": after.Status, "reply": after.Reply, "hidden_reason": after.HiddenReason})
			})
			return nil, apix.Err(err)
		})
}
