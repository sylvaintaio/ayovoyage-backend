package boarding

import (
	"context"
	"net/http"
	"sort"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/booking"
	"busvoyage/internal/departure"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/store"
)

func (h *Handler) registerOnboardSales(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("record-onboard-sales", http.MethodPost, "/v1/agent/departures/{id}/sales", tagAgent,
		"Vente à bord : en direct ou envoi des ventes faites sans réseau", apix.Staff, fieldRoles...),
		"Réservé au contrôleur du départ (et à l'administrateur), depuis un terminal enregistré. Prix fixe du trajet "+
			"(sans remise), espèces encaissées par le contrôleur, billets émis et embarqués aussitôt. `client_sale_id` rend "+
			"l'envoi idempotent. Sans réseau (`offline`), seules les places du quota sont vendues ; si l'une a été prise "+
			"entre-temps, une autre place libre est attribuée, sinon la vente est enregistrée en anomalie (`issue`)."),
		func(ctx context.Context, in *struct {
			ID       uuid.UUID `path:"id"`
			DeviceID string    `header:"X-Device-ID" required:"true"`
			Body     struct {
				Sales []booking.OnboardSaleInput `json:"sales" minItems:"1" maxItems:"100"`
			}
		}) (*struct{ Body []booking.OnboardSaleResult }, error) {
			p := auth.MustFromContext(ctx)
			if h.Bookings == nil {
				return nil, huma.Error503ServiceUnavailable("Vente à bord indisponible")
			}
			sales := in.Body.Sales
			sort.SliceStable(sales, func(i, j int) bool { return sales[i].SoldAt.Before(sales[j].SoldAt) })
			out := &struct{ Body []booking.OnboardSaleResult }{Body: []booking.OnboardSaleResult{}}
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
					return huma.Error403Forbidden("Seul le contrôleur du départ vend à bord")
				}
				for _, s := range sales {
					r, err := h.Bookings.SellOnboard(ctx, tx, q, booking.OnboardSeller{Principal: p, DeviceID: dev}, d, s)
					if err != nil {
						// Une vente en direct refusée (plus de place, trajet invalide) : l'erreur est renvoyée telle quelle.
						return err
					}
					out.Body = append(out.Body, r)
				}
				return nil
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-agent-onboard", http.MethodGet, "/v1/agent/departures/{id}/onboard", tagAgent,
		"Mes ventes à bord et ma caisse", apix.Staff, fieldRoles...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body departure.OnboardSummary }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body departure.OnboardSummary }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				d, acc, err := h.loadDeparture(ctx, q, p, in.ID, false)
				if err != nil {
					return err
				}
				if !acc.controller && p.Role != auth.RoleCompanyAdmin {
					return huma.Error403Forbidden("Réservé au contrôleur du départ")
				}
				out.Body, err = departure.LoadOnboard(ctx, q, d)
				return err
			})
			return out, apix.Err(err)
		})
}
