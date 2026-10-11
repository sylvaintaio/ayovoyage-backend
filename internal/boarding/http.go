// Package boarding : API de l'application Agent / Convoyeur / Chauffeur
// (départs affectés, manifeste, scan des billets, synchronisation hors ligne).
package boarding

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/departure"
	"busvoyage/internal/jobs"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
	"busvoyage/internal/ticketsign"
)

const tagAgent = "Agent — Embarquement"

var fieldRoles = []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleBoardingAgent, auth.RoleDriver}

type Handler struct {
	DB         *database.DB
	Signer     *ticketsign.Signer
	Queue      *jobs.Queue
	Departures *departure.Handler
}

// ---------------------------------------------------------------- DTO

type ManifestEntry struct {
	SeatLabel        string     `json:"seat_label"`
	PassengerName    string     `json:"passenger_name"`
	PassengerPhone   string     `json:"passenger_phone,omitempty"`
	BookingReference string     `json:"booking_reference"`
	TicketID         uuid.UUID  `json:"ticket_id"`
	TicketNumber     string     `json:"ticket_number"`
	TicketStatus     string     `json:"ticket_status" enum:"valid,used,cancelled"`
	BoardedAt        *time.Time `json:"boarded_at,omitempty"`
	CategoryCode     string     `json:"category_code"`
	BoardingSeq      int32      `json:"boarding_seq" doc:"Arrêt de montée (indice dans stops)"`
	AlightingSeq     int32      `json:"alighting_seq" doc:"Arrêt de descente (indice dans stops)"`
}

type Manifest struct {
	DepartureID uuid.UUID                 `json:"departure_id"`
	Expected    int                       `json:"expected"`
	Boarded     int                       `json:"boarded"`
	Missing     int                       `json:"missing"`
	Passengers  []ManifestEntry           `json:"passengers"`
	Stops       []departure.DepartureStop `json:"stops" doc:"Arrêts du départ, dans l'ordre : en attente et descentes par arrêt"`
	Visits      []StopVisit               `json:"visits" doc:"Journal des arrêts déjà déclarés"`
	GeneratedAt time.Time                 `json:"generated_at"`
}

// AgentDevice : terminal enregistré par un responsable et rattaché à l'agent.
type AgentDevice struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Platform string    `json:"platform"`
}

type ScanResult struct {
	Result        string     `json:"result" enum:"boarded,already_used,invalid_signature,wrong_departure,cancelled,not_found,conflict"`
	Message       string     `json:"message"`
	TicketID      *uuid.UUID `json:"ticket_id,omitempty"`
	SeatLabel     string     `json:"seat_label,omitempty"`
	PassengerName string     `json:"passenger_name,omitempty"`
	BoardedAt     *time.Time `json:"boarded_at,omitempty"`
	ClientScanID  string     `json:"client_scan_id,omitempty"`
	BoardingSeq   *int32     `json:"boarding_seq,omitempty" doc:"Arrêt de montée réservé"`
	AlightingSeq  *int32     `json:"alighting_seq,omitempty" doc:"Arrêt de descente : à afficher en gros"`
	// Le voyageur monte à un autre arrêt que celui réservé : accepté, mais signalé au contrôleur.
	BoardingMismatch bool `json:"boarding_mismatch,omitempty"`
}

var messages = map[string]string{
	"boarded":           "Embarquement validé",
	"already_used":      "Billet déjà utilisé",
	"invalid_signature": "QR code invalide ou falsifié",
	"wrong_departure":   "Ce billet n'est pas pour ce départ",
	"cancelled":         "Billet annulé",
	"not_found":         "Billet introuvable",
	"conflict":          "Billet déjà scanné sur un autre appareil",
}

// ---------------------------------------------------------------- Scan

type scanInput struct {
	QRPayload    string
	ScannedAt    time.Time
	Offline      bool
	ClientScanID string
	StopSeq      *int32 // arrêt où le contrôle a lieu (facultatif)
}

// scan valide un billet pour un départ. Règle hors ligne : le premier scan gagne.
func (h *Handler) scan(ctx context.Context, tx pgx.Tx, q *store.Queries, p auth.Principal, departureID uuid.UUID, deviceID *uuid.UUID, in scanInput) (ScanResult, error) {
	res := ScanResult{ClientScanID: in.ClientScanID}
	record := func(result string, ticketID *uuid.UUID) (ScanResult, error) {
		res.Result, res.Message = result, messages[result]
		_, err := q.InsertBoardingScan(ctx, store.InsertBoardingScanParams{
			CompanyID: p.CompanyID, DepartureID: departureID, TicketID: ticketID, StaffID: p.ID, DeviceID: deviceID,
			Result: result, Offline: in.Offline, ScannedAt: in.ScannedAt, ClientScanID: apix.NilIfEmpty(in.ClientScanID),
			StopSeq: in.StopSeq,
		})
		if database.IsNotFound(err) { // scan déjà synchronisé (idempotence)
			err = nil
		}
		return res, err
	}

	// Lot déjà synchronisé : on renvoie le résultat d'origine sans rejouer le scan.
	if in.ClientScanID != "" && deviceID != nil {
		prev, err := q.GetBoardingScanByClientID(ctx, store.GetBoardingScanByClientIDParams{DeviceID: deviceID, ClientScanID: &in.ClientScanID})
		if err == nil {
			res.Result, res.Message, res.TicketID = prev.Result, messages[prev.Result], prev.TicketID
			return res, nil
		}
		if !database.IsNotFound(err) {
			return res, err
		}
	}

	payload, err := h.Signer.Verify(in.QRPayload)
	if err != nil {
		return record("invalid_signature", nil)
	}
	if payload.CompanyID != p.CompanyID {
		return record("not_found", nil)
	}
	t, err := q.GetTicketForUpdate(ctx, payload.TicketID)
	if database.IsNotFound(err) {
		return record("not_found", nil)
	}
	if err != nil {
		return res, err
	}
	res.TicketID, res.SeatLabel, res.PassengerName, res.BoardedAt = &t.ID, t.SeatLabel, t.PassengerName, t.BoardedAt
	res.BoardingSeq, res.AlightingSeq = &t.BoardingSeq, &t.AlightingSeq
	res.BoardingMismatch = in.StopSeq != nil && *in.StopSeq != t.BoardingSeq
	if t.DepartureID != departureID {
		return record("wrong_departure", &t.ID)
	}
	switch t.Status {
	case "cancelled":
		return record("cancelled", &t.ID)
	case "used":
		if in.Offline {
			return record("conflict", &t.ID)
		}
		return record("already_used", &t.ID)
	}
	staffID := p.ID
	at := in.ScannedAt
	if err := q.MarkTicketUsed(ctx, store.MarkTicketUsedParams{ID: t.ID, BoardedAt: &at, BoardedBy: &staffID}); err != nil {
		return res, err
	}
	res.BoardedAt = &at
	if err := h.Queue.InsertTx(ctx, tx, jobs.LoyaltyEarnArgs{TicketID: t.ID}); err != nil {
		return res, err
	}
	return record("boarded", &t.ID)
}

// device vérifie le terminal déclaré (en-tête X-Device-ID) : actif et appartenant à l'agent.
func (h *Handler) device(ctx context.Context, q *store.Queries, p auth.Principal, raw string) (*uuid.UUID, error) {
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, huma.Error400BadRequest("X-Device-ID invalide")
	}
	d, err := q.GetDevice(ctx, id)
	if err != nil || d.CompanyID != p.CompanyID || d.StaffID != p.ID {
		return nil, huma.Error403Forbidden("Terminal non enregistré pour cet agent")
	}
	if !d.IsActive {
		return nil, huma.Error403Forbidden("Terminal désactivé : contactez votre responsable")
	}
	return &id, q.TouchDevice(ctx, id)
}

// ---------------------------------------------------------------- Routes

func (h *Handler) Register(api huma.API) {
	h.registerStopJournal(api)
	huma.Register(api, apix.Op("list-agent-departures", http.MethodGet, "/v1/agent/departures", tagAgent,
		"Départs du jour (affectés à moi ou à mon agence)", apix.Staff, fieldRoles...),
		func(ctx context.Context, in *struct {
			Date string `query:"date" format:"date" doc:"Par défaut : aujourd'hui"`
		}) (*struct{ Body []departure.AdminDeparture }, error) {
			p := auth.MustFromContext(ctx)
			day := time.Now().In(departure.LocalTZ)
			day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, departure.LocalTZ)
			if in.Date != "" {
				var err error
				if day, err = time.ParseInLocation(time.DateOnly, in.Date, departure.LocalTZ); err != nil {
					return nil, huma.Error422UnprocessableEntity("Date invalide")
				}
			}
			params := store.ListDeparturesParams{CompanyID: p.CompanyID, FromTs: day, ToTs: day.AddDate(0, 0, 1)}
			switch {
			case p.Role == auth.RoleBoardingAgent || p.Role == auth.RoleDriver:
				if !inCrewWindow(day, time.Now()) { // équipage : de la veille au lendemain uniquement
					return &struct{ Body []departure.AdminDeparture }{Body: []departure.AdminDeparture{}}, nil
				}
				params.StaffID = &p.ID
			case p.AgencyID != nil:
				params.OriginAgencyID = p.AgencyID
			}
			out := &struct{ Body []departure.AdminDeparture }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, err = departure.ListForStaff(ctx, q, params)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("get-departure-manifest", http.MethodGet, "/v1/agent/departures/{id}/manifest", tagAgent,
		"Manifeste : passagers attendus, embarqués, absents", apix.Staff, fieldRoles...),
		"À télécharger avant le départ pour fonctionner hors connexion."),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body Manifest }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body Manifest }{Body: Manifest{DepartureID: in.ID, Passengers: []ManifestEntry{}, GeneratedAt: time.Now()}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				_, acc, err := h.loadDeparture(ctx, q, p, in.ID, false)
				if err != nil {
					return err
				}
				if out.Body.Stops, err = departure.LoadStops(ctx, q, in.ID); err != nil {
					return err
				}
				if out.Body.Visits, err = loadVisits(ctx, q, in.ID); err != nil {
					return err
				}
				// Chef d'agence : seulement les voyageurs qui montent ou descendent à son agence.
				atAgency := map[int32]bool{}
				if acc.agency != nil {
					for _, st := range out.Body.Stops {
						atAgency[int32(st.Seq)] = st.AgencyID != nil && *st.AgencyID == *acc.agency
					}
				}
				rows, err := q.GetDepartureManifest(ctx, in.ID)
				for _, r := range rows {
					e := ManifestEntry(r)
					if acc.agency != nil && !atAgency[e.BoardingSeq] && !atAgency[e.AlightingSeq] {
						continue
					}
					if !acc.phones {
						e.PassengerPhone = "" // chauffeur, équipage : pas de coordonnées des voyageurs
					}
					out.Body.Passengers = append(out.Body.Passengers, e)
					if r.TicketStatus != "cancelled" {
						out.Body.Expected++
					}
					if r.TicketStatus == "used" {
						out.Body.Boarded++
					}
				}
				out.Body.Missing = out.Body.Expected - out.Body.Boarded
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("agent-change-departure-status", http.MethodPost, "/v1/agent/departures/{id}/status", tagAgent,
		"Déclarer l'ouverture de l'embarquement, le départ ou l'arrivée", apix.Staff, fieldRoles...),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Status string `json:"status" enum:"boarding,departed,arrived"`
			}
		}) (*struct {
			Body struct {
				Status string `json:"status"`
			}
		}, error) {
			p := auth.MustFromContext(ctx)
			if err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				_, _, err := h.loadDeparture(ctx, q, p, in.ID, false)
				return err
			}); err != nil {
				return nil, apix.Err(err)
			}
			d, _, err := h.Departures.ChangeStatus(ctx, p, in.ID, in.Body.Status, "")
			if err != nil {
				return nil, apix.Err(err)
			}
			out := &struct {
				Body struct {
					Status string `json:"status"`
				}
			}{}
			out.Body.Status = d.Status
			return out, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("scan-ticket", http.MethodPost, "/v1/agent/departures/{id}/scan", tagAgent,
		"Scanner un billet (en ligne)", apix.Staff, fieldRoles...),
		"Vérifie le terminal, la signature du QR, le départ, le statut, puis passe le billet à « embarqué ». "+
			"Un billet déjà utilisé est signalé avec l'heure du premier passage. Chaque scan est journalisé."),
		func(ctx context.Context, in *struct {
			ID       uuid.UUID `path:"id"`
			DeviceID string    `header:"X-Device-ID" required:"true" doc:"Identifiant du terminal enregistré"`
			Body     struct {
				QRPayload string `json:"qr_payload" minLength:"10" maxLength:"2000"`
				StopSeq   *int32 `json:"stop_seq,omitempty" minimum:"0" doc:"Arrêt où a lieu le contrôle : signale une montée hors de l'arrêt réservé"`
			}
		}) (*struct{ Body ScanResult }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body ScanResult }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
				dev, err := h.device(ctx, q, p, in.DeviceID)
				if err != nil {
					return err
				}
				if _, _, err := h.loadDeparture(ctx, q, p, in.ID, true); err != nil {
					return err
				}
				out.Body, err = h.scan(ctx, tx, q, p, in.ID, dev, scanInput{QRPayload: in.Body.QRPayload, ScannedAt: time.Now(), StopSeq: in.Body.StopSeq})
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("sync-offline-scans", http.MethodPost, "/v1/agent/departures/{id}/sync", tagAgent,
		"Synchroniser les scans effectués hors connexion", apix.Staff, fieldRoles...),
		"Les scans sont rejoués dans l'ordre chronologique. Règle : le premier scan gagne ; les doublons sont signalés en `conflict`. "+
			"`client_scan_id` rend l'envoi idempotent (renvoyer un lot déjà synchronisé est sans effet)."),
		func(ctx context.Context, in *struct {
			ID       uuid.UUID `path:"id"`
			DeviceID string    `header:"X-Device-ID" required:"true"`
			Body     struct {
				Scans []struct {
					ClientScanID string    `json:"client_scan_id" minLength:"1" maxLength:"64"`
					QRPayload    string    `json:"qr_payload" minLength:"10" maxLength:"2000"`
					ScannedAt    time.Time `json:"scanned_at"`
					StopSeq      *int32    `json:"stop_seq,omitempty" minimum:"0"`
				} `json:"scans" minItems:"1" maxItems:"500"`
			}
		}) (*struct{ Body []ScanResult }, error) {
			p := auth.MustFromContext(ctx)
			scans := in.Body.Scans
			sort.SliceStable(scans, func(i, j int) bool { return scans[i].ScannedAt.Before(scans[j].ScannedAt) })
			out := &struct{ Body []ScanResult }{Body: []ScanResult{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
				dev, err := h.device(ctx, q, p, in.DeviceID)
				if err != nil {
					return err
				}
				if _, _, err := h.loadDeparture(ctx, q, p, in.ID, false); err != nil {
					return err
				}
				now := time.Now()
				for _, s := range scans {
					at := s.ScannedAt
					if at.After(now) { // horloge du terminal en avance
						at = now
					}
					r, err := h.scan(ctx, tx, q, p, in.ID, dev, scanInput{QRPayload: s.QRPayload, ScannedAt: at, Offline: true, ClientScanID: s.ClientScanID, StopSeq: s.StopSeq})
					if err != nil {
						return err
					}
					out.Body = append(out.Body, r)
				}
				return nil
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("get-my-device", http.MethodGet, "/v1/agent/device", tagAgent,
		"Vérifier le terminal de ce téléphone", apix.Staff, fieldRoles...),
		"À l'activation de l'application Agent : le terminal doit exister, appartenir à l'agent connecté et être actif (sinon 403)."),
		func(ctx context.Context, in *struct {
			DeviceID string `header:"X-Device-ID" required:"true"`
		}) (*struct{ Body AgentDevice }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body AgentDevice }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				id, err := h.device(ctx, q, p, in.DeviceID)
				if err != nil {
					return err
				}
				d, err := q.GetDevice(ctx, *id)
				out.Body = AgentDevice{ID: d.ID, Name: d.Name, Platform: d.Platform}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("get-ticket-keys", http.MethodGet, "/v1/agent/ticket-keys", tagAgent,
		"Clés publiques de vérification des QR codes", apix.Staff, fieldRoles...),
		"Permet à l'application Agent de vérifier la signature des billets sans connexion."),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []ticketsign.PublicKey }, error) {
			return &struct{ Body []ticketsign.PublicKey }{Body: h.Signer.PublicKeys()}, nil
		})
}
