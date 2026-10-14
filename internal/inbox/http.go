// Package inbox : notifications des applications (téléphones inscrits, boîte de réception, non lus).
package inbox

import (
	"context"
	"encoding/json"
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

const tag = "Notifications"

type Handler struct {
	DB *database.DB
}

// InboxMessage : notification de la boîte de réception.
type InboxMessage struct {
	ID        uuid.UUID         `json:"id"`
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Link      string            `json:"link,omitempty" doc:"Écran de l'application à ouvrir"`
	Data      map[string]string `json:"data,omitempty"`
	Read      bool              `json:"read"`
	CreatedAt time.Time         `json:"created_at"`
}

type InboxPage struct {
	Messages []InboxMessage `json:"messages"`
	Unread   int32          `json:"unread" doc:"Nombre de non lus (pastille sur l'icône de l'application)"`
}

type PushDeviceInput struct {
	Token    string `json:"token" minLength:"20" maxLength:"4096" doc:"Jeton Firebase Cloud Messaging du téléphone"`
	Platform string `json:"platform" enum:"android,ios,web"`
}

func toMessage(m store.InboxMessage) InboxMessage {
	out := InboxMessage{ID: m.ID, Kind: m.Kind, Title: m.Title, Body: m.Body, Link: m.Link, Read: m.ReadAt != nil, CreatedAt: m.CreatedAt}
	_ = json.Unmarshal(m.Data, &out.Data)
	return out
}

// owner : destinataire selon la session (client ou personnel).
func owner(p auth.Principal) (customer, staff *uuid.UUID) {
	id := p.ID
	if p.IsStaff() {
		return nil, &id
	}
	return &id, nil
}

func (h *Handler) Register(api huma.API) {
	// Mêmes routes pour l'app Voyageur (/v1/me) et l'app Agent (/v1/agent).
	for _, side := range []struct {
		prefix, app, opPrefix string
		access                apix.Access
	}{
		{"/v1/me", "voyageur", "my", apix.Customer},
		{"/v1/agent", "agent", "agent", apix.Staff},
	} {
		h.register(api, side.prefix, side.app, side.opPrefix, side.access)
	}
}

func (h *Handler) register(api huma.API, prefix, app, op string, access apix.Access) {
	huma.Register(api, apix.WithDescription(apix.Op(op+"-register-push-device", http.MethodPost, prefix+"/push-devices", tag,
		"Inscrire ce téléphone aux notifications", access),
		"À appeler après la connexion et à chaque nouveau jeton Firebase. Un jeton déjà inscrit par un autre compte "+
			"(changement d'utilisateur sur le téléphone) passe à ce compte."),
		func(ctx context.Context, in *struct{ Body PushDeviceInput }) (*struct{}, error) {
			p := auth.MustFromContext(ctx)
			customer, staff := owner(p)
			var company *uuid.UUID
			if p.IsStaff() {
				company = &p.CompanyID
			}
			// Portée système : le jeton peut appartenir à un autre compte (même téléphone).
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				_, err := q.UpsertPushDevice(ctx, store.UpsertPushDeviceParams{Token: in.Body.Token, Platform: in.Body.Platform,
					App: app, CustomerID: customer, StaffID: staff, CompanyID: company})
				return err
			})
			return nil, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op(op+"-unregister-push-device", http.MethodPost, prefix+"/push-devices/remove", tag,
		"Désinscrire ce téléphone (déconnexion)", access),
		"Plus aucune notification de ce compte sur ce téléphone."),
		func(ctx context.Context, in *struct {
			Body struct {
				Token string `json:"token" minLength:"1" maxLength:"4096"`
			}
		}) (*struct{}, error) {
			p := auth.MustFromContext(ctx)
			customer, staff := owner(p)
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				return q.DeletePushDevice(ctx, store.DeletePushDeviceParams{Token: in.Body.Token, CustomerID: customer, StaffID: staff})
			})
			return nil, apix.Err(err)
		})

	huma.Register(api, apix.Op(op+"-inbox", http.MethodGet, prefix+"/inbox", tag, "Mes notifications", access),
		func(ctx context.Context, in *struct {
			Before time.Time `query:"before" doc:"Pagination : messages plus anciens que cette date"`
			Limit  int       `query:"limit" minimum:"1" maximum:"100" default:"30"`
		}) (*struct{ Body InboxPage }, error) {
			p := auth.MustFromContext(ctx)
			customer, staff := owner(p)
			out := &struct{ Body InboxPage }{Body: InboxPage{Messages: []InboxMessage{}}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var before *time.Time
				if !in.Before.IsZero() {
					before = &in.Before
				}
				rows, err := q.ListInbox(ctx, store.ListInboxParams{CustomerID: customer, StaffID: staff, Before: before, Lim: int32(in.Limit)})
				if err != nil {
					return err
				}
				for _, r := range rows {
					out.Body.Messages = append(out.Body.Messages, toMessage(r))
				}
				out.Body.Unread, err = q.CountUnreadInbox(ctx, store.CountUnreadInboxParams{CustomerID: customer, StaffID: staff})
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op(op+"-inbox-unread", http.MethodGet, prefix+"/inbox/unread", tag, "Nombre de notifications non lues", access),
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				Unread int32 `json:"unread"`
			}
		}, error) {
			p := auth.MustFromContext(ctx)
			customer, staff := owner(p)
			out := &struct {
				Body struct {
					Unread int32 `json:"unread"`
				}
			}{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body.Unread, err = q.CountUnreadInbox(ctx, store.CountUnreadInboxParams{CustomerID: customer, StaffID: staff})
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op(op+"-inbox-read", http.MethodPost, prefix+"/inbox/{id}/read", tag, "Marquer une notification comme lue", access),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct {
			Body struct {
				Unread int32 `json:"unread"`
			}
		}, error) {
			return h.markRead(ctx, &in.ID)
		})

	huma.Register(api, apix.Op(op+"-inbox-read-all", http.MethodPost, prefix+"/inbox/read-all", tag, "Tout marquer comme lu", access),
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				Unread int32 `json:"unread"`
			}
		}, error) {
			return h.markRead(ctx, nil)
		})
}

// markRead : un message (id) ou tous ; renvoie le nombre de non lus restant (pastille de l'icône).
func (h *Handler) markRead(ctx context.Context, id *uuid.UUID) (*struct {
	Body struct {
		Unread int32 `json:"unread"`
	}
}, error) {
	p := auth.MustFromContext(ctx)
	customer, staff := owner(p)
	out := &struct {
		Body struct {
			Unread int32 `json:"unread"`
		}
	}{}
	err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		var err error
		if id != nil {
			_, err = q.MarkInboxRead(ctx, store.MarkInboxReadParams{ID: *id, CustomerID: customer, StaffID: staff})
		} else {
			_, err = q.MarkAllInboxRead(ctx, store.MarkAllInboxReadParams{CustomerID: customer, StaffID: staff})
		}
		if err != nil {
			return err
		}
		out.Body.Unread, err = q.CountUnreadInbox(ctx, store.CountUnreadInboxParams{CustomerID: customer, StaffID: staff})
		return err
	})
	return out, apix.Err(err)
}
