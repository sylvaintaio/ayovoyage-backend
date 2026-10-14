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
)

// StopVisit : arrêt déclaré par le contrôleur (prévu ou imprévu), heures réelles et position.
type StopVisit struct {
	ID         uuid.UUID  `json:"id"`
	Seq        *int32     `json:"seq,omitempty" doc:"Arrêt prévu (indice dans stops) ; absent : arrêt imprévu"`
	Reason     string     `json:"reason" enum:"planned,police,breakdown,puncture,pause,traffic,other"`
	Note       string     `json:"note,omitempty"`
	ArrivedAt  time.Time  `json:"arrived_at"`
	DepartedAt *time.Time `json:"departed_at,omitempty"`
	Latitude   *float64   `json:"latitude,omitempty"`
	Longitude  *float64   `json:"longitude,omitempty"`
	Accuracy   *float32   `json:"accuracy,omitempty" doc:"Précision du GPS (mètres)"`
	Mocked     bool       `json:"mocked,omitempty" doc:"Position simulée signalée par le téléphone"`
	ClientID   string     `json:"client_id,omitempty"`
}

func toVisit(v store.DepartureStopVisit) StopVisit {
	out := StopVisit{ID: v.ID, Seq: v.Seq, Reason: v.Reason, Note: v.Note, ArrivedAt: v.ArrivedAt, DepartedAt: v.DepartedAt,
		Latitude: v.ArrivalLat, Longitude: v.ArrivalLng, Accuracy: v.ArrivalAccuracy, Mocked: v.LocationMocked}
	if v.ClientID != nil {
		out.ClientID = *v.ClientID
	}
	return out
}

func loadVisits(ctx context.Context, q *store.Queries, departureID uuid.UUID) ([]StopVisit, error) {
	rows, err := q.ListStopVisits(ctx, departureID)
	if err != nil {
		return nil, err
	}
	out := make([]StopVisit, 0, len(rows))
	for _, v := range rows {
		out = append(out, toVisit(v))
	}
	return out, nil
}

// StopEventInput : événement du journal, envoyé en direct ou après une coupure de réseau.
type StopEventInput struct {
	ClientID      string    `json:"client_id" minLength:"1" maxLength:"64" doc:"Identifiant unique côté téléphone : renvoyer un événement est sans effet"`
	Type          string    `json:"type" enum:"arrival,departure"`
	VisitClientID string    `json:"visit_client_id,omitempty" maxLength:"64" doc:"Départ : client_id de l'arrivée correspondante (absent : dernier arrêt ouvert)"`
	Seq           *int32    `json:"seq,omitempty" minimum:"0" doc:"Arrivée à un arrêt prévu ; absent : arrêt imprévu (reason obligatoire)"`
	Reason        string    `json:"reason,omitempty" enum:"planned,police,breakdown,puncture,pause,traffic,other,"`
	Note          string    `json:"note,omitempty" maxLength:"500"`
	At            time.Time `json:"at" doc:"Heure réelle de l'événement (plafonnée à l'heure du serveur)"`
	Latitude      *float64  `json:"latitude,omitempty" minimum:"-90" maximum:"90"`
	Longitude     *float64  `json:"longitude,omitempty" minimum:"-180" maximum:"180"`
	Accuracy      *float32  `json:"accuracy,omitempty" minimum:"0"`
	Mocked        bool      `json:"mocked,omitempty"`
}

type StopEventResult struct {
	ClientID string     `json:"client_id"`
	Result   string     `json:"result" enum:"recorded,duplicate,ignored" doc:"ignored : départ sans arrêt ouvert"`
	Visit    *StopVisit `json:"visit,omitempty"`
}

func (h *Handler) registerStopJournal(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("record-stop-events", http.MethodPost, "/v1/agent/departures/{id}/stop-events", tagAgent,
		"Journal des arrêts : arrivée, départ, arrêt imprévu", apix.Staff, fieldRoles...),
		"Réservé au contrôleur du départ (et à l'administrateur), depuis un terminal enregistré. Les événements sont traités "+
			"dans l'ordre chronologique ; `client_id` rend l'envoi idempotent (file hors connexion). Une arrivée clôt l'arrêt "+
			"précédent resté ouvert. Effets : départ du premier arrêt → départ « parti » ; arrivée au dernier arrêt → « arrivé »."),
		func(ctx context.Context, in *struct {
			ID       uuid.UUID `path:"id"`
			DeviceID string    `header:"X-Device-ID" required:"true"`
			Body     struct {
				Events []StopEventInput `json:"events" minItems:"1" maxItems:"200"`
			}
		}) (*struct{ Body []StopEventResult }, error) {
			p := auth.MustFromContext(ctx)
			events := in.Body.Events
			sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
			out := &struct{ Body []StopEventResult }{Body: []StopEventResult{}}
			var nextStatus string
			err := h.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
				dev, err := h.device(ctx, q, p, in.DeviceID)
				if err != nil {
					return err
				}
				d, acc, err := h.loadDeparture(ctx, q, p, in.ID, false)
				if err != nil {
					return err
				}
				if !acc.controller && p.Role != auth.RoleCompanyAdmin {
					return huma.Error403Forbidden("Seul le contrôleur du départ tient le journal des arrêts")
				}
				last, err := q.DepartureLastSeq(ctx, d.ID)
				if err != nil {
					return err
				}
				// Agence de chaque arrêt : chargement et déchargement des colis au passage du car.
				stopRows, err := q.ListDepartureStops(ctx, d.ID)
				if err != nil {
					return err
				}
				agencyAt := map[int32]uuid.UUID{}
				for _, st := range stopRows {
					if st.AgencyID != nil {
						agencyAt[st.Seq] = *st.AgencyID
					}
				}
				parcels := h.parcels()
				now := time.Now()
				for _, e := range events {
					at := e.At
					if at.After(now) { // horloge du téléphone en avance
						at = now
					}
					res := StopEventResult{ClientID: e.ClientID}
					switch e.Type {
					case "arrival":
						if prev, err := q.GetStopVisitByClientID(ctx, store.GetStopVisitByClientIDParams{DeviceID: dev, ClientID: &e.ClientID}); err == nil {
							v := toVisit(prev)
							res.Result, res.Visit = "duplicate", &v
							break
						} else if !database.IsNotFound(err) {
							return err
						}
						reason := e.Reason
						if e.Seq != nil {
							if *e.Seq < 0 || *e.Seq > last {
								return huma.Error422UnprocessableEntity("Arrêt inconnu sur ce départ")
							}
							reason = "planned"
						} else if reason == "" || reason == "planned" {
							return huma.Error422UnprocessableEntity("Arrêt imprévu : indiquez le motif")
						}
						if err := q.CloseOpenStopVisits(ctx, store.CloseOpenStopVisitsParams{DepartureID: d.ID, At: &at}); err != nil {
							return err
						}
						clientID := e.ClientID
						v, err := q.InsertStopVisit(ctx, store.InsertStopVisitParams{CompanyID: d.CompanyID, DepartureID: d.ID, Seq: e.Seq,
							Reason: reason, Note: e.Note, ArrivedAt: at, ArrivalLat: e.Latitude, ArrivalLng: e.Longitude,
							ArrivalAccuracy: e.Accuracy, LocationMocked: e.Mocked, RecordedBy: p.ID, DeviceID: dev, ClientID: &clientID})
						if err != nil {
							return err
						}
						visit := toVisit(v)
						res.Result, res.Visit = "recorded", &visit
						if e.Seq != nil && *e.Seq == last {
							nextStatus = "arrived"
						}
						if ag, ok := agencyAtSeq(agencyAt, e.Seq); ok && parcels != nil {
							if err := parcels.OnStopArrival(ctx, q, d.ID, ag); err != nil {
								return err
							}
						}
					case "departure":
						// Départ déjà reçu (renvoi après coupure) : sans effet, même si un autre arrêt est ouvert depuis.
						if prev, err := q.GetStopVisitByDepartureClientID(ctx, store.GetStopVisitByDepartureClientIDParams{
							DeviceID: dev, DepartureClientID: &e.ClientID}); err == nil {
							v := toVisit(prev)
							res.Result, res.Visit = "duplicate", &v
							break
						} else if !database.IsNotFound(err) {
							return err
						}
						target, err := h.openVisit(ctx, q, d.ID, dev, e.VisitClientID)
						if err != nil {
							return err
						}
						if target == nil {
							res.Result = "ignored"
							break
						}
						if target.DepartedAt != nil {
							v := toVisit(*target)
							res.Result, res.Visit = "duplicate", &v
							break
						}
						clientID := e.ClientID
						v, err := q.DepartStopVisit(ctx, store.DepartStopVisitParams{ID: target.ID, DepartedAt: &at,
							DepartureLat: e.Latitude, DepartureLng: e.Longitude, DepartureClientID: &clientID})
						if err != nil {
							return err
						}
						visit := toVisit(v)
						res.Result, res.Visit = "recorded", &visit
						if v.Seq != nil && *v.Seq == 0 {
							nextStatus = "departed"
						}
						// Voyageurs qui attendent plus loin : prévenus (notification « votre car arrive »).
						if v.Seq != nil && *v.Seq < last && h.Queue != nil {
							if err := h.Queue.InsertTx(ctx, tx, jobs.DepartureProgressArgs{DepartureID: d.ID, Seq: int(*v.Seq),
								DepartedAt: at}); err != nil {
								return err
							}
						}
						if ag, ok := agencyAtSeq(agencyAt, v.Seq); ok && parcels != nil {
							if err := parcels.OnStopDeparture(ctx, q, d.ID, ag); err != nil {
								return err
							}
						}
					}
					out.Body = append(out.Body, res)
				}
				return nil
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			// Statut du départ suivi automatiquement (transition déjà faite ou impossible : sans effet).
			if nextStatus != "" {
				_, _, _ = h.Departures.ChangeStatus(ctx, p, in.ID, nextStatus, "")
			}
			return out, nil
		})
}

// openVisit : arrêt visé par un départ — celui de l'arrivée indiquée, sinon le dernier arrêt encore ouvert.
func (h *Handler) openVisit(ctx context.Context, q *store.Queries, departureID uuid.UUID, dev *uuid.UUID, visitClientID string) (*store.DepartureStopVisit, error) {
	if visitClientID != "" {
		v, err := q.GetStopVisitByClientID(ctx, store.GetStopVisitByClientIDParams{DeviceID: dev, ClientID: &visitClientID})
		if database.IsNotFound(err) {
			return nil, nil
		}
		if err != nil || v.DepartureID != departureID {
			return nil, err
		}
		return &v, nil
	}
	rows, err := q.ListStopVisits(ctx, departureID)
	if err != nil {
		return nil, err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].DepartedAt == nil {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func agencyAtSeq(m map[int32]uuid.UUID, seq *int32) (uuid.UUID, bool) {
	if seq == nil {
		return uuid.Nil, false
	}
	ag, ok := m[*seq]
	return ag, ok
}

// parcels : colis et bagages (absent dans certains tests).
func (h *Handler) parcels() departure.ParcelHook {
	if h.Departures == nil {
		return nil
	}
	return h.Departures.Parcels
}
