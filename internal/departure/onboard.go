package departure

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/itinerary"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/segment"
	"busvoyage/internal/store"
)

// ---------------------------------------------------------------- Places réservées au contrôleur

// OnboardSeat : place du quota du contrôleur (vendable seulement à bord, y compris sans réseau).
type OnboardSeat struct {
	SeatID     uuid.UUID  `json:"seat_id"`
	SeatLabel  string     `json:"seat_label"`
	CategoryID *uuid.UUID `json:"category_id,omitempty"`
	Surcharge  int64      `json:"surcharge" doc:"Supplément de catégorie et de siège, ajouté au prix du tronçon"`
}

// LoadOnboardSeats : quota du contrôleur, avec le supplément de chaque place.
func LoadOnboardSeats(ctx context.Context, q *store.Queries, companyID, departureID uuid.UUID) ([]OnboardSeat, error) {
	rows, err := q.ListOnboardSeats(ctx, departureID)
	if err != nil {
		return nil, err
	}
	cats, err := q.ListSeatCategories(ctx, companyID)
	if err != nil {
		return nil, err
	}
	surcharge := map[uuid.UUID]int64{}
	for _, c := range cats {
		surcharge[c.ID] = c.SurchargeAmount
	}
	out := make([]OnboardSeat, 0, len(rows))
	for _, r := range rows {
		s := OnboardSeat{SeatID: r.ID, CategoryID: r.CategoryID, Surcharge: r.ExtraSurcharge}
		if r.SeatLabel != nil {
			s.SeatLabel = *r.SeatLabel
		}
		if r.CategoryID != nil {
			s.Surcharge += surcharge[*r.CategoryID]
		}
		out = append(out, s)
	}
	return out, nil
}

// applyDefaultQuota : quota de la compagnie appliqué à un nouveau départ.
func applyDefaultQuota(ctx context.Context, q *store.Queries, d store.Departure, by *uuid.UUID) (int, error) {
	co, err := q.GetCompany(ctx, d.CompanyID)
	if err != nil {
		return 0, err
	}
	return SetOnboardQuota(ctx, q, d, int(co.OnboardQuotaSeats), by)
}

// SetOnboardQuota porte le quota du contrôleur à n places : ajoute des places libres sur tout le trajet
// (les moins chères, au fond du car) ou rend à la vente des places du quota encore jamais vendues.
// Renvoie le nombre de places obtenu (moins que n si le car est déjà plein).
func SetOnboardQuota(ctx context.Context, q *store.Queries, d store.Departure, n int, by *uuid.UUID) (int, error) {
	current, err := q.ListOnboardSeats(ctx, d.ID)
	if err != nil {
		return 0, err
	}
	count := len(current)
	// Trop de places : on rend d'abord celles de l'avant du car, si personne ne les occupe.
	for i := 0; i < len(current) && count > n; i++ {
		busy, err := q.SeatHasActiveBooking(ctx, store.SeatHasActiveBookingParams{DepartureID: d.ID, SeatCellID: current[i].ID})
		if err != nil {
			return 0, err
		}
		if busy {
			continue
		}
		if err := q.ReleaseOnboardSeat(ctx, store.ReleaseOnboardSeatParams{DepartureID: d.ID, SeatCellID: current[i].ID}); err != nil {
			return 0, err
		}
		count--
	}
	if count >= n {
		return count, nil
	}
	last, err := q.DepartureLastSeq(ctx, d.ID)
	if err != nil {
		return 0, err
	}
	cells, err := q.GetDepartureSeatMap(ctx, store.GetDepartureSeatMapParams{DepartureID: d.ID, LayoutID: d.LayoutID, FromSeq: 0, ToSeq: last})
	if err != nil {
		return 0, err
	}
	cats, err := q.ListSeatCategories(ctx, d.CompanyID)
	if err != nil {
		return 0, err
	}
	surcharge := map[uuid.UUID]int64{}
	for _, c := range cats {
		surcharge[c.ID] = c.SurchargeAmount
	}
	price := func(c store.GetDepartureSeatMapRow) int64 {
		if c.CategoryID == nil {
			return c.ExtraSurcharge
		}
		return surcharge[*c.CategoryID] + c.ExtraSurcharge
	}
	free := slices.DeleteFunc(slices.Clone(cells), func(c store.GetDepartureSeatMapRow) bool {
		return c.Kind != "seat" || c.State != "available" || c.CategoryID == nil
	})
	slices.SortStableFunc(free, func(a, b store.GetDepartureSeatMapRow) int {
		return cmp.Or(cmp.Compare(price(a), price(b)), cmp.Compare(b.RowIndex, a.RowIndex), cmp.Compare(a.ColIndex, b.ColIndex))
	})
	for _, c := range free {
		if count >= n {
			break
		}
		if err := q.BlockOnboardSeat(ctx, store.BlockOnboardSeatParams{DepartureID: d.ID, SeatCellID: c.ID,
			CompanyID: d.CompanyID, BlockedBy: by}); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

// ---------------------------------------------------------------- Ventes et caisse de bord

// OnboardSale : vente faite à bord par le contrôleur.
type OnboardSale struct {
	BookingID    uuid.UUID `json:"booking_id"`
	Reference    string    `json:"reference"`
	ContactName  string    `json:"contact_name"`
	ContactPhone string    `json:"contact_phone,omitempty"`
	BoardingSeq  int32     `json:"boarding_seq"`
	AlightingSeq int32     `json:"alighting_seq"`
	Seats        int32     `json:"seats"`
	SeatLabels   string    `json:"seat_labels"`
	Amount       int64     `json:"amount"`
	Cash         int64     `json:"cash" doc:"Espèces encaissées"`
	Status       string    `json:"status"`
	SoldOffline  bool      `json:"sold_offline"`
	SellerName   string    `json:"seller_name"`
	SoldAt       time.Time `json:"sold_at"`
}

// OnboardIssue : vente sans réseau qui n'a pas pu être placée (plus de siège libre sur le tronçon).
type OnboardIssue struct {
	ID         uuid.UUID  `json:"id"`
	Reason     string     `json:"reason"`
	Amount     int64      `json:"amount"`
	StaffName  string     `json:"staff_name"`
	SoldAt     time.Time  `json:"sold_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
}

// CashHandover : caisse de bord remise à une agence.
type CashHandover struct {
	AgencyID       uuid.UUID `json:"agency_id"`
	AgencyName     string    `json:"agency_name"`
	ExpectedCash   int64     `json:"expected_cash"`
	CountedCash    int64     `json:"counted_cash"`
	Difference     int64     `json:"difference"`
	SalesCount     int32     `json:"sales_count"`
	Note           string    `json:"note,omitempty"`
	ReceivedByName string    `json:"received_by_name"`
	ReceivedAt     time.Time `json:"received_at"`
}

// OnboardSummary : ventes à bord d'un départ, caisse du contrôleur et remise à l'agence.
type OnboardSummary struct {
	QuotaSeats []OnboardSeat  `json:"quota_seats"`
	Sales      []OnboardSale  `json:"sales"`
	Cash       int64          `json:"cash" doc:"Espèces encaissées à bord (à remettre)"`
	SalesCount int32          `json:"sales_count"`
	Issues     []OnboardIssue `json:"issues"`
	Handover   *CashHandover  `json:"handover,omitempty"`
}

// LoadOnboard : ventes à bord et caisse d'un départ.
func LoadOnboard(ctx context.Context, q *store.Queries, d store.Departure) (OnboardSummary, error) {
	out := OnboardSummary{Sales: []OnboardSale{}, Issues: []OnboardIssue{}}
	var err error
	if out.QuotaSeats, err = LoadOnboardSeats(ctx, q, d.CompanyID, d.ID); err != nil {
		return out, err
	}
	sales, err := q.ListOnboardSales(ctx, d.ID)
	if err != nil {
		return out, err
	}
	for _, s := range sales {
		out.Sales = append(out.Sales, OnboardSale{BookingID: s.ID, Reference: s.Reference, ContactName: s.ContactName,
			ContactPhone: s.ContactPhone, BoardingSeq: s.BoardingSeq, AlightingSeq: s.AlightingSeq, Seats: s.Seats,
			SeatLabels: s.SeatLabels, Amount: s.TotalAmount, Cash: s.Cash, Status: s.Status, SoldOffline: s.SoldOffline,
			SellerName: s.SellerName, SoldAt: s.CreatedAt})
	}
	total, err := q.OnboardCashTotal(ctx, d.ID)
	if err != nil {
		return out, err
	}
	out.Cash, out.SalesCount = total.Cash, total.SalesCount
	issues, err := q.ListOnboardSaleIssues(ctx, d.ID)
	if err != nil {
		return out, err
	}
	for _, i := range issues {
		out.Issues = append(out.Issues, OnboardIssue{ID: i.ID, Reason: i.Reason, Amount: i.Amount, StaffName: i.StaffName,
			SoldAt: i.SoldAt, ResolvedAt: i.ResolvedAt, Resolution: i.Resolution})
	}
	h, err := q.GetOnboardCashHandover(ctx, d.ID)
	if err == nil {
		out.Handover = &CashHandover{AgencyID: h.AgencyID, AgencyName: h.AgencyName, ExpectedCash: h.ExpectedCash,
			CountedCash: h.CountedCash, Difference: *h.Difference, SalesCount: h.SalesCount, Note: h.Note,
			ReceivedByName: h.ReceivedByName, ReceivedAt: h.ReceivedAt}
	} else if !database.IsNotFound(err) {
		return out, err
	}
	return out, nil
}

func (h *Handler) registerOnboard(api huma.API) {
	planners := []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager}
	cashiers := []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier}

	// Départ du personnel, limité à son agence pour un chef d'agence ou un caissier.
	load := func(ctx context.Context, q *store.Queries, p auth.Principal, id uuid.UUID) (store.Departure, error) {
		d, err := q.GetDeparture(ctx, id)
		if database.IsNotFound(err) || (err == nil && d.CompanyID != p.CompanyID) {
			return d, apix.NotFound("Départ")
		}
		return d, err
	}

	huma.Register(api, apix.Op("get-departure-onboard", http.MethodGet, "/v1/admin/departures/{id}/onboard", tagAdmin,
		"Ventes à bord, caisse du contrôleur et places réservées", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleFinance),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body OnboardSummary }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body OnboardSummary }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := load(ctx, q, p, in.ID)
				if err != nil {
					return err
				}
				out.Body, err = LoadOnboard(ctx, q, d)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("set-onboard-quota", http.MethodPut, "/v1/admin/departures/{id}/onboard-quota", tagAdmin,
		"Places réservées à la vente à bord", apix.Staff, planners...),
		"Ajoute des places libres (les moins chères, au fond du car) ou rend à la vente des places du quota jamais vendues. "+
			"Le résultat peut compter moins de places que demandé si le car est plein."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Seats int `json:"seats" minimum:"0" maximum:"20"`
			}
		}) (*struct{ Body OnboardSummary }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body OnboardSummary }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := load(ctx, q, p, in.ID)
				if err != nil {
					return err
				}
				if d.Status == "cancelled" || d.Status == "arrived" {
					return huma.Error409Conflict("Départ terminé")
				}
				before, err := q.ListOnboardSeats(ctx, d.ID)
				if err != nil {
					return err
				}
				got, err := SetOnboardQuota(ctx, q, d, in.Body.Seats, &p.ID)
				if err != nil {
					return err
				}
				if err := apix.Audit(ctx, q, &p.CompanyID, "departure.onboard_quota", "departure", d.ID.String(),
					map[string]any{"seats": len(before)}, map[string]any{"requested": in.Body.Seats, "seats": got}); err != nil {
					return err
				}
				out.Body, err = LoadOnboard(ctx, q, d)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("hand-over-onboard-cash", http.MethodPost, "/v1/admin/departures/{id}/onboard/handover", tagAdmin,
		"Recevoir la caisse du contrôleur", apix.Staff, cashiers...),
		"L'agent qui reçoit l'argent saisit le montant compté ; un écart doit être expliqué. La remise entre dans la caisse "+
			"du jour de l'agence. Une seule remise par départ."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				AgencyID    *uuid.UUID `json:"agency_id,omitempty" doc:"Par défaut : l'agence de l'employé"`
				CountedCash int64      `json:"counted_cash" minimum:"0"`
				Note        string     `json:"note,omitempty" maxLength:"500"`
			}
		}) (*struct{ Body OnboardSummary }, error) {
			p := auth.MustFromContext(ctx)
			agencyID := in.Body.AgencyID
			if agencyID == nil {
				agencyID = p.AgencyID
			}
			if agencyID == nil {
				return nil, huma.Error422UnprocessableEntity("Agence qui reçoit la caisse requise (agency_id)")
			}
			if p.Role != auth.RoleCompanyAdmin && (p.AgencyID == nil || *p.AgencyID != *agencyID) {
				return nil, huma.Error403Forbidden("Vous ne pouvez recevoir une caisse que pour votre agence")
			}
			out := &struct{ Body OnboardSummary }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := load(ctx, q, p, in.ID)
				if err != nil {
					return err
				}
				if _, err := q.GetAgency(ctx, store.GetAgencyParams{ID: *agencyID, CompanyID: p.CompanyID}); err != nil {
					if database.IsNotFound(err) {
						return huma.Error422UnprocessableEntity("Agence inconnue")
					}
					return err
				}
				total, err := q.OnboardCashTotal(ctx, d.ID)
				if err != nil {
					return err
				}
				if in.Body.CountedCash != total.Cash && len(in.Body.Note) < 5 {
					return huma.Error422UnprocessableEntity("Écart de caisse : expliquez-le dans la remarque")
				}
				if _, err := q.InsertOnboardCashHandover(ctx, store.InsertOnboardCashHandoverParams{CompanyID: p.CompanyID,
					DepartureID: d.ID, ControllerID: d.AgentID, AgencyID: *agencyID, ExpectedCash: total.Cash,
					CountedCash: in.Body.CountedCash, SalesCount: total.SalesCount, Note: in.Body.Note, ReceivedBy: p.ID}); err != nil {
					if database.UniqueViolation(err) != "" {
						return huma.Error409Conflict("La caisse de ce départ a déjà été remise")
					}
					return err
				}
				if err := apix.Audit(ctx, q, &p.CompanyID, "departure.onboard_cash_handover", "departure", d.ID.String(), nil,
					map[string]any{"agency_id": agencyID, "expected": total.Cash, "counted": in.Body.CountedCash}); err != nil {
					return err
				}
				out.Body, err = LoadOnboard(ctx, q, d)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("resolve-onboard-issue", http.MethodPost, "/v1/admin/departures/{id}/onboard/issues/{issue_id}/resolve", tagAdmin,
		"Clore une anomalie de vente à bord", apix.Staff, planners...),
		func(ctx context.Context, in *struct {
			ID      uuid.UUID `path:"id"`
			IssueID uuid.UUID `path:"issue_id"`
			Body    struct {
				Resolution string `json:"resolution" minLength:"3" maxLength:"500" doc:"Ce qui a été fait (remboursé, billet émis à la main…)"`
			}
		}) (*struct{ Body OnboardSummary }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body OnboardSummary }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := load(ctx, q, p, in.ID)
				if err != nil {
					return err
				}
				if _, err := q.ResolveOnboardSaleIssue(ctx, store.ResolveOnboardSaleIssueParams{ID: in.IssueID, DepartureID: d.ID,
					ResolvedBy: &p.ID, Resolution: in.Body.Resolution}); err != nil {
					if database.IsNotFound(err) {
						return apix.NotFound("Anomalie")
					}
					return err
				}
				if err := apix.Audit(ctx, q, &p.CompanyID, "departure.onboard_issue_resolved", "departure", d.ID.String(), nil,
					map[string]any{"issue_id": in.IssueID, "resolution": in.Body.Resolution}); err != nil {
					return err
				}
				out.Body, err = LoadOnboard(ctx, q, d)
				return err
			})
			return out, apix.Err(err)
		})
}

// SegmentFare : prix d'un trajet (avant supplément de siège), pour vendre sans réseau.
type SegmentFare struct {
	BoardingSeq  int   `json:"boarding_seq"`
	AlightingSeq int   `json:"alighting_seq"`
	UnitPrice    int64 `json:"unit_price"`
}

// OnboardKit : ce que le contrôleur télécharge pour vendre sans réseau.
type OnboardKit struct {
	QuotaSeats []OnboardSeat `json:"quota_seats" doc:"Places réservées à la vente à bord (seules vendables sans réseau)"`
	Fares      []SegmentFare `json:"fares" doc:"Prix de chaque trajet possible (montée à n'importe quel arrêt)"`
}

// LoadOnboardKit : places du quota et grille des prix du départ.
func LoadOnboardKit(ctx context.Context, q *store.Queries, d store.Departure) (OnboardKit, error) {
	kit := OnboardKit{Fares: []SegmentFare{}}
	var err error
	if kit.QuotaSeats, err = LoadOnboardSeats(ctx, q, d.CompanyID, d.ID); err != nil {
		return kit, err
	}
	stops, err := segment.LoadStops(ctx, q, d.ID)
	if err != nil {
		return kit, err
	}
	route, err := q.GetRoute(ctx, store.GetRouteParams{ID: d.RouteID, CompanyID: d.CompanyID})
	if err != nil {
		return kit, err
	}
	pr, err := segment.Pricing(ctx, q, route)
	if err != nil {
		return kit, err
	}
	for i := range stops {
		for j := i + 1; j < len(stops); j++ {
			if !stops[j].CanAlight || stops[i].CityID == stops[j].CityID {
				continue
			}
			kit.Fares = append(kit.Fares, SegmentFare{BoardingSeq: stops[i].Seq, AlightingSeq: stops[j].Seq,
				UnitPrice: itinerary.SegmentPrice(pr, d.BasePrice, stops[i].CityID, stops[j].CityID)})
		}
	}
	return kit, nil
}
