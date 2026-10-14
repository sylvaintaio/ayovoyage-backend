package departure

import (
	"context"
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

// TripProgress : suivi du voyage pour le voyageur (arrêts prévus, arrêts réels, arrêts imprévus, retard).
type TripProgress struct {
	DepartureStatus string          `json:"departure_status"`
	BoardingSeq     int32           `json:"boarding_seq" doc:"Arrêt de montée du voyageur"`
	AlightingSeq    int32           `json:"alighting_seq" doc:"Arrêt de descente du voyageur"`
	Stops           []DepartureStop `json:"stops"`
	Visits          []StopVisitView `json:"visits" doc:"Arrêts déclarés par le contrôleur, heures réelles (arrêts imprévus : motif)"`
	DelayMinutes    int             `json:"delay_minutes" doc:"Retard constaté au dernier départ d'arrêt (0 : à l'heure ou pas encore parti)"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// delayOf : retard au dernier arrêt quitté (heure réelle − heure prévue − pause).
func delayOf(stops []DepartureStop, visits []StopVisitView) int {
	byseq := map[int]DepartureStop{}
	for _, s := range stops {
		byseq[s.Seq] = s
	}
	delay := 0
	for _, v := range visits {
		if v.Seq == nil || v.DepartedAt == nil {
			continue
		}
		st, ok := byseq[int(*v.Seq)]
		if !ok {
			continue
		}
		planned := st.ScheduledAt.Add(time.Duration(st.PauseMinutes) * time.Minute)
		delay = max(int(v.DepartedAt.Sub(planned).Minutes()), 0)
	}
	return delay
}

func (h *Handler) registerTracking(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("get-my-booking-trip", http.MethodGet, "/v1/bookings/{id}/trip", tagPublic,
		"Suivi du voyage (arrêts et retards)", apix.Customer),
		"Arrêts du départ avec heures prévues, et arrêts réellement effectués déclarés par le contrôleur "+
			"(heures d'arrivée et de départ, arrêts imprévus avec leur motif). Mis à jour en direct."),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body TripProgress }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body TripProgress }{}
			err := h.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				b, err := q.GetBooking(ctx, in.ID)
				if err != nil || b.CustomerID == nil || *b.CustomerID != p.ID {
					return apix.NotFound("Réservation")
				}
				d, err := q.GetDeparture(ctx, b.DepartureID)
				if err != nil {
					return err
				}
				out.Body = TripProgress{DepartureStatus: d.Status, BoardingSeq: b.BoardingSeq, AlightingSeq: b.AlightingSeq, UpdatedAt: time.Now()}
				if out.Body.Stops, err = LoadStops(ctx, q, d.ID); err != nil {
					return err
				}
				if out.Body.Visits, err = LoadVisits(ctx, q, d.ID); err != nil {
					return err
				}
				out.Body.DelayMinutes = delayOf(out.Body.Stops, out.Body.Visits)
				return nil
			})
			return out, apix.Err(err)
		})
}
