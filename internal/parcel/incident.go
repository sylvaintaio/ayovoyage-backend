package parcel

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/notify"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/refgen"
	"busvoyage/internal/store"
)

const tagIncidents = "Réclamations"

var incidentRoles = []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleParcelAgent, auth.RoleCashier}

var categoryText = map[string]string{
	"not_received": "Non reçu", "delayed": "Retard", "damaged": "Endommagé", "opened": "Ouvert / contenu manquant",
	"wrong_recipient": "Mauvais destinataire", "wrong_agency": "Mauvaise agence", "info_error": "Erreur d'information",
	"lost": "Perdu", "other": "Autre",
}

var incidentStatusText = map[string]string{
	"open": "ouvert", "in_progress": "en cours de traitement", "resolved": "résolu", "rejected": "clos sans suite",
}

type IncidentCreated struct {
	Reference string `json:"reference"`
	Status    string `json:"status"`
}

type IncidentSummary struct {
	ID               uuid.UUID `json:"id"`
	Reference        string    `json:"reference"`
	SubjectType      string    `json:"subject_type" enum:"parcel,baggage,booking"`
	Category         string    `json:"category"`
	Priority         string    `json:"priority,omitempty"`
	Status           string    `json:"status" enum:"open,in_progress,resolved,rejected"`
	ReporterName     string    `json:"reporter_name,omitempty"`
	TrackingNumber   *string   `json:"tracking_number,omitempty"`
	BaggageTag       *string   `json:"baggage_tag,omitempty"`
	BookingReference *string   `json:"booking_reference,omitempty"`
	AssignedName     *string   `json:"assigned_name,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type IncidentEvent struct {
	AuthorType string    `json:"author_type"`
	AuthorName string    `json:"author_name"`
	Action     string    `json:"action"`
	Note       string    `json:"note"`
	IsPublic   bool      `json:"is_public"`
	CreatedAt  time.Time `json:"created_at"`
}

type PublicIncident struct {
	Reference        string          `json:"reference"`
	SubjectType      string          `json:"subject_type"`
	Category         string          `json:"category"`
	Status           string          `json:"status"`
	Description      string          `json:"description"`
	Resolution       string          `json:"resolution,omitempty"`
	TrackingNumber   *string         `json:"tracking_number,omitempty"`
	BookingReference *string         `json:"booking_reference,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	ResolvedAt       *time.Time      `json:"resolved_at,omitempty"`
	Events           []IncidentEvent `json:"events"`
}

type IncidentDetail struct {
	IncidentSummary
	Description   string          `json:"description"`
	ReporterType  string          `json:"reporter_type"`
	ReporterPhone string          `json:"reporter_phone"`
	ParcelID      *uuid.UUID      `json:"parcel_id,omitempty"`
	BookingID     *uuid.UUID      `json:"booking_id,omitempty"`
	AssignedTo    *uuid.UUID      `json:"assigned_to,omitempty"`
	Resolution    string          `json:"resolution"`
	ResolvedAt    *time.Time      `json:"resolved_at,omitempty"`
	Attachments   []uuid.UUID     `json:"attachments" doc:"Photos : GET /v1/admin/attachments/{id}"`
	Events        []IncidentEvent `json:"events" doc:"Historique complet, notes internes comprises"`
}

type newIncident struct {
	companyID     uuid.UUID
	subject       string
	parcelID      *uuid.UUID
	baggageID     *uuid.UUID
	bookingID     *uuid.UUID
	category      string
	description   string
	reporterType  string
	reporterName  string
	reporterPhone string
	customerID    *uuid.UUID
	priority      string
	photos        []string
	staffID       *uuid.UUID
}

// decodePhoto : data URL image → (type, octets), 2 Mo au plus.
func decodePhoto(dataURL string) (string, []byte, error) {
	head, b64, ok := strings.Cut(dataURL, ";base64,")
	ct := strings.TrimPrefix(head, "data:")
	if !ok || (ct != "image/jpeg" && ct != "image/png" && ct != "image/webp") {
		return "", nil, huma.Error422UnprocessableEntity("Photo invalide (JPEG, PNG ou WebP)")
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil, huma.Error422UnprocessableEntity("Photo illisible")
	}
	if len(data) > 2<<20 {
		return "", nil, huma.Error422UnprocessableEntity("Photo trop lourde (2 Mo au plus)")
	}
	return ct, data, nil
}

// openIncident crée le dossier, joint les photos et prévient le déclarant.
func (s *Service) openIncident(ctx context.Context, q *store.Queries, n newIncident) (IncidentCreated, error) {
	if n.priority == "" {
		n.priority = "normal"
		if n.category == "lost" || n.category == "opened" {
			n.priority = "high"
		}
	}
	i, err := q.InsertIncident(ctx, store.InsertIncidentParams{CompanyID: n.companyID, Reference: refgen.Incident(), SubjectType: n.subject,
		ParcelID: n.parcelID, BaggageID: n.baggageID, BookingID: n.bookingID, Category: n.category, Description: strings.TrimSpace(n.description),
		ReporterType: n.reporterType, ReporterName: strings.TrimSpace(n.reporterName), ReporterPhone: n.reporterPhone,
		ReporterCustomerID: n.customerID, Priority: n.priority})
	if err != nil {
		return IncidentCreated{}, err
	}
	for _, ph := range n.photos {
		ct, data, err := decodePhoto(ph)
		if err != nil {
			return IncidentCreated{}, err
		}
		if _, err := q.InsertAttachment(ctx, store.InsertAttachmentParams{CompanyID: n.companyID, IncidentID: &i.ID, ContentType: ct, Data: data}); err != nil {
			return IncidentCreated{}, err
		}
	}
	authorType, authorID, authorName := n.reporterType, n.customerID, i.ReporterName
	if n.staffID != nil { // saisi par l'équipe : l'auteur est l'agent
		authorType, authorID, authorName = "staff", n.staffID, s.staffName(ctx, q, *n.staffID)
	}
	if err := q.InsertIncidentEvent(ctx, store.InsertIncidentEventParams{CompanyID: n.companyID, IncidentID: i.ID, AuthorType: authorType,
		AuthorID: authorID, AuthorName: authorName, Action: "created", Note: categoryText[n.category], IsPublic: true}); err != nil {
		return IncidentCreated{}, err
	}
	s.smsReporter(ctx, q, i.CompanyID, i.ReporterPhone, i.ReporterCustomerID, "incident_opened", fmt.Sprintf(
		"réclamation %s enregistrée (%s). Nous revenons vers vous rapidement. Suivi : %s/reclamation/%s",
		i.Reference, strings.ToLower(categoryText[n.category]), s.Cfg.PublicWebURL, i.Reference))
	return IncidentCreated{Reference: i.Reference, Status: i.Status}, nil
}

func (s *Service) smsReporter(ctx context.Context, q *store.Queries, companyID uuid.UUID, phoneNumber string, customerID *uuid.UUID, template, text string) {
	company, _ := q.GetCompany(ctx, companyID)
	msg := company.Name + " : " + text
	err := s.SMS.SendSMS(ctx, phoneNumber, msg)
	status, errMsg := notify.DeliveryStatus(s.SMS, err)
	if err != nil {
		slog.Error("SMS réclamation", "err", err)
	}
	_ = q.InsertNotification(ctx, store.InsertNotificationParams{CompanyID: &companyID, CustomerID: customerID, Channel: "sms",
		Template: template, Recipient: phoneNumber, Status: status, Error: errMsg, Body: msg})
}

func toEvents(rows []store.IncidentEvent) []IncidentEvent {
	out := make([]IncidentEvent, 0, len(rows))
	for _, e := range rows {
		out = append(out, IncidentEvent{AuthorType: e.AuthorType, AuthorName: e.AuthorName, Action: e.Action, Note: e.Note,
			IsPublic: e.IsPublic, CreatedAt: e.CreatedAt})
	}
	return out
}

func (s *Service) publicIncident(ctx context.Context, q *store.Queries, i store.GetIncidentRow) (PublicIncident, error) {
	evs, err := q.ListIncidentEvents(ctx, store.ListIncidentEventsParams{IncidentID: i.ID, IncludeInternal: false})
	return PublicIncident{Reference: i.Reference, SubjectType: i.SubjectType, Category: i.Category, Status: i.Status,
		Description: i.Description, Resolution: i.Resolution, TrackingNumber: i.TrackingNumber, BookingReference: i.BookingReference,
		CreatedAt: i.CreatedAt, ResolvedAt: i.ResolvedAt, Events: toEvents(evs)}, err
}

func (s *Service) incidentDetail(ctx context.Context, q *store.Queries, p auth.Principal, id uuid.UUID) (IncidentDetail, error) {
	i, err := q.GetIncident(ctx, id)
	if database.IsNotFound(err) || (err == nil && i.CompanyID != p.CompanyID) {
		return IncidentDetail{}, apix.NotFound("Réclamation")
	}
	if err != nil {
		return IncidentDetail{}, err
	}
	evs, err := q.ListIncidentEvents(ctx, store.ListIncidentEventsParams{IncidentID: i.ID, IncludeInternal: true})
	if err != nil {
		return IncidentDetail{}, err
	}
	atts, err := q.ListIncidentAttachmentIDs(ctx, &i.ID)
	d := IncidentDetail{
		IncidentSummary: IncidentSummary{ID: i.ID, Reference: i.Reference, SubjectType: i.SubjectType, Category: i.Category, Priority: i.Priority,
			Status: i.Status, ReporterName: i.ReporterName, TrackingNumber: i.TrackingNumber, BaggageTag: i.TagNumber,
			BookingReference: i.BookingReference, AssignedName: i.AssignedName, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt},
		Description: i.Description, ReporterType: i.ReporterType, ReporterPhone: i.ReporterPhone, ParcelID: i.ParcelID, BookingID: i.BookingID,
		AssignedTo: i.AssignedTo, Resolution: i.Resolution, ResolvedAt: i.ResolvedAt, Attachments: []uuid.UUID{}, Events: toEvents(evs),
	}
	for _, a := range atts {
		d.Attachments = append(d.Attachments, a.ID)
	}
	return d, err
}

func (s *Service) staffName(ctx context.Context, q *store.Queries, id uuid.UUID) string {
	st, err := q.GetStaff(ctx, id)
	if err != nil {
		return ""
	}
	return st.FullName
}

func (s *Service) registerIncidents(api huma.API) {
	huma.Register(api, apix.Op("list-incidents", http.MethodGet, "/v1/admin/incidents", tagIncidents, "Réclamations", apix.Staff, incidentRoles...),
		func(ctx context.Context, in *struct {
			apix.Page
			Status  string `query:"status" enum:"open,in_progress,resolved,rejected,"`
			Subject string `query:"subject" enum:"parcel,baggage,booking,"`
		}) (*struct{ Body []IncidentSummary }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []IncidentSummary }{Body: []IncidentSummary{}}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListIncidents(ctx, store.ListIncidentsParams{CompanyID: p.CompanyID, Status: apix.NilIfEmpty(in.Status),
					Subject: apix.NilIfEmpty(in.Subject), Lim: in.Limit, Off: in.Offset})
				for _, r := range rows {
					out.Body = append(out.Body, IncidentSummary{ID: r.ID, Reference: r.Reference, SubjectType: r.SubjectType, Category: r.Category,
						Priority: r.Priority, Status: r.Status, ReporterName: r.ReporterName, TrackingNumber: r.TrackingNumber, BaggageTag: r.TagNumber,
						BookingReference: r.BookingReference, AssignedName: r.AssignedName, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-incident", http.MethodGet, "/v1/admin/incidents/{id}", tagIncidents, "Dossier de réclamation", apix.Staff, incidentRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body IncidentDetail }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body IncidentDetail }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, err = s.incidentDetail(ctx, q, p, in.ID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-incident", http.MethodPost, "/v1/admin/incidents", tagIncidents,
		"Ouvrir une réclamation (au guichet)", apix.Staff, incidentRoles...),
		"Pour un client qui se présente en agence, ou un constat interne (colis endommagé, bagage manquant au déchargement)."),
		func(ctx context.Context, in *struct {
			Body struct {
				TrackingNumber string   `json:"tracking_number,omitempty" maxLength:"40"`
				BaggageTag     string   `json:"baggage_tag,omitempty" maxLength:"20"`
				Category       string   `json:"category" enum:"not_received,delayed,damaged,opened,wrong_recipient,wrong_agency,info_error,lost,other"`
				Description    string   `json:"description" minLength:"10" maxLength:"2000"`
				ReporterName   string   `json:"reporter_name" minLength:"2" maxLength:"120"`
				ReporterPhone  string   `json:"reporter_phone" minLength:"8" maxLength:"20"`
				Priority       string   `json:"priority,omitempty" enum:"low,normal,high,urgent,"`
				Photos         []string `json:"photos,omitempty" maxItems:"3"`
			}
		}) (*struct{ Body IncidentDetail }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			ph, err := s.normPhone(b.ReporterPhone, "déclarant")
			if err != nil {
				return nil, err
			}
			out := &struct{ Body IncidentDetail }{}
			err = s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				staffID := p.ID
				n := newIncident{companyID: p.CompanyID, category: b.Category, description: b.Description, reporterType: "staff",
					reporterName: b.ReporterName, reporterPhone: ph, priority: b.Priority, photos: b.Photos, staffID: &staffID}
				switch {
				case b.TrackingNumber != "":
					parcel, err := q.GetParcelByTrackingForUpdate(ctx, TrackingFromScan(b.TrackingNumber))
					if err != nil || parcel.CompanyID != p.CompanyID {
						return apix.NotFound("Colis")
					}
					n.subject, n.parcelID = "parcel", &parcel.ID
					if err := s.event(ctx, q, parcel, "incident", parcel.CurrentAgencyID, nil, &staffID, categoryText[b.Category]); err != nil {
						return err
					}
				case b.BaggageTag != "":
					t, err := q.GetBaggageByTagForUpdate(ctx, b.BaggageTag)
					if err != nil || t.CompanyID != p.CompanyID {
						return apix.NotFound("Bagage")
					}
					n.subject, n.baggageID = "baggage", &t.ID
				default:
					return huma.Error422UnprocessableEntity("Indiquez le numéro du colis ou l'étiquette du bagage")
				}
				created, err := s.openIncident(ctx, q, n)
				if err != nil {
					return err
				}
				id, err := q.GetIncidentIDByReference(ctx, created.Reference)
				if err != nil {
					return err
				}
				out.Body, err = s.incidentDetail(ctx, q, p, id)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("update-incident", http.MethodPut, "/v1/admin/incidents/{id}", tagIncidents,
		"Traiter une réclamation", apix.Staff, incidentRoles...),
		"Statut, priorité, responsable et résolution. Le déclarant est prévenu par SMS quand le dossier est résolu ou clos."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Status     string     `json:"status" enum:"open,in_progress,resolved,rejected"`
				Priority   string     `json:"priority" enum:"low,normal,high,urgent"`
				AssignedTo *uuid.UUID `json:"assigned_to,omitempty"`
				Resolution string     `json:"resolution,omitempty" maxLength:"2000" doc:"Obligatoire pour résoudre ou clore ; envoyée au déclarant"`
			}
		}) (*struct{ Body IncidentDetail }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			if (b.Status == "resolved" || b.Status == "rejected") && len(strings.TrimSpace(b.Resolution)) < 5 {
				return nil, huma.Error422UnprocessableEntity("Décrivez la solution apportée au client")
			}
			out := &struct{ Body IncidentDetail }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetIncident(ctx, in.ID)
				if err != nil || before.CompanyID != p.CompanyID {
					return apix.NotFound("Réclamation")
				}
				if b.AssignedTo != nil {
					if st, err := q.GetStaff(ctx, *b.AssignedTo); err != nil || st.CompanyID != p.CompanyID {
						return huma.Error422UnprocessableEntity("Responsable inconnu")
					}
				}
				i, err := q.UpdateIncident(ctx, store.UpdateIncidentParams{ID: before.ID, Status: b.Status, Priority: b.Priority,
					AssignedTo: b.AssignedTo, Resolution: strings.TrimSpace(b.Resolution)})
				if err != nil {
					return err
				}
				me := s.staffName(ctx, q, p.ID)
				log := func(action, note string, public bool) error {
					return q.InsertIncidentEvent(ctx, store.InsertIncidentEventParams{CompanyID: i.CompanyID, IncidentID: i.ID, AuthorType: "staff",
						AuthorID: &p.ID, AuthorName: me, Action: action, Note: note, IsPublic: public})
				}
				if before.Status != i.Status {
					note := "Dossier " + incidentStatusText[i.Status]
					if i.Resolution != "" && (i.Status == "resolved" || i.Status == "rejected") {
						note += " : " + i.Resolution
					}
					if err := log("status", note, true); err != nil {
						return err
					}
					if i.Status != "open" {
						s.smsReporter(ctx, q, i.CompanyID, i.ReporterPhone, i.ReporterCustomerID, "incident_"+i.Status,
							fmt.Sprintf("votre réclamation %s est %s. %s", i.Reference, incidentStatusText[i.Status], i.Resolution))
					}
				}
				if before.Priority != i.Priority {
					if err := log("priority", "Priorité : "+i.Priority, false); err != nil {
						return err
					}
				}
				if fmt.Sprint(before.AssignedTo) != fmt.Sprint(i.AssignedTo) && i.AssignedTo != nil {
					if err := log("assigned", "Confié à "+s.staffName(ctx, q, *i.AssignedTo), false); err != nil {
						return err
					}
				}
				if err := apix.Audit(ctx, q, &p.CompanyID, "incident.updated", "incident", i.ID.String(),
					map[string]any{"status": before.Status, "priority": before.Priority}, map[string]any{"status": i.Status, "priority": i.Priority}); err != nil {
					return err
				}
				out.Body, err = s.incidentDetail(ctx, q, p, i.ID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("comment-incident", http.MethodPost, "/v1/admin/incidents/{id}/comments", tagIncidents,
		"Ajouter un message au dossier", apix.Staff, incidentRoles...),
		"`public: true` : visible par le client (et envoyé par SMS) ; sinon note interne."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Message string `json:"message" minLength:"2" maxLength:"2000"`
				Public  bool   `json:"public"`
			}
		}) (*struct{ Body IncidentDetail }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body IncidentDetail }{}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				i, err := q.GetIncident(ctx, in.ID)
				if err != nil || i.CompanyID != p.CompanyID {
					return apix.NotFound("Réclamation")
				}
				if err := q.InsertIncidentEvent(ctx, store.InsertIncidentEventParams{CompanyID: i.CompanyID, IncidentID: i.ID, AuthorType: "staff",
					AuthorID: &p.ID, AuthorName: s.staffName(ctx, q, p.ID), Action: "comment", Note: in.Body.Message, IsPublic: in.Body.Public}); err != nil {
					return err
				}
				if in.Body.Public {
					s.smsReporter(ctx, q, i.CompanyID, i.ReporterPhone, i.ReporterCustomerID, "incident_message",
						fmt.Sprintf("réclamation %s : %s", i.Reference, in.Body.Message))
				}
				out.Body, err = s.incidentDetail(ctx, q, p, i.ID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-attachment", http.MethodGet, "/v1/admin/attachments/{id}", tagIncidents, "Photo jointe", apix.Staff, incidentRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct {
			ContentType  string `header:"Content-Type"`
			CacheControl string `header:"Cache-Control"`
			Body         []byte
		}, error) {
			p := auth.MustFromContext(ctx)
			var a store.Attachment
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				a, err = q.GetAttachment(ctx, in.ID)
				if err != nil || a.CompanyID != p.CompanyID {
					return apix.NotFound("Photo")
				}
				return nil
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct {
				ContentType  string `header:"Content-Type"`
				CacheControl string `header:"Cache-Control"`
				Body         []byte
			}{ContentType: a.ContentType, CacheControl: "private, max-age=3600", Body: a.Data}, nil
		})
}
