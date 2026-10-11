package payment

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

// Clôture de caisse quotidienne par agence : l'agent compte les espèces, le
// système calcule ce qui devrait s'y trouver (encaissements − remboursements
// versés en espèces) et enregistre l'écart. Une clôture est définitive.

const tagCash = "Caisse"

var beninTZ = time.FixedZone("WAT", 3600)

type CashierTotal struct {
	StaffID    uuid.UUID `json:"staff_id"`
	FullName   string    `json:"full_name"`
	SalesCount int32     `json:"sales_count"`
	Amount     int64     `json:"amount"`
}

type CashClosure struct {
	ID           uuid.UUID `json:"id"`
	AgencyID     uuid.UUID `json:"agency_id"`
	AgencyName   string    `json:"agency_name,omitempty"`
	BusinessDate string    `json:"business_date" format:"date"`
	CashIn       int64     `json:"cash_in"`
	CashOut      int64     `json:"cash_out"`
	ExpectedCash int64     `json:"expected_cash"`
	CountedCash  int64     `json:"counted_cash"`
	Difference   int64     `json:"difference" doc:"Compté − attendu (négatif = manquant)"`
	SalesCount   int32     `json:"sales_count"`
	Note         string    `json:"note"`
	ClosedBy     string    `json:"closed_by"`
	ClosedAt     time.Time `json:"closed_at"`
}

type CashSummary struct {
	AgencyID       uuid.UUID      `json:"agency_id"`
	BusinessDate   string         `json:"business_date" format:"date"`
	CashIn         int64          `json:"cash_in" doc:"Encaissements en espèces (ventes et modifications au guichet)"`
	SalesCount     int32          `json:"sales_count"`
	CashOut        int64          `json:"cash_out" doc:"Remboursements versés en espèces par l'agence"`
	RefundsCount   int32          `json:"refunds_count"`
	OnboardIn      int64          `json:"onboard_in" doc:"Caisses de bord remises par les contrôleurs (montant compté)"`
	HandoversCount int32          `json:"handovers_count"`
	ExpectedCash   int64          `json:"expected_cash" doc:"cash_in + onboard_in - cash_out"`
	ByCashier      []CashierTotal `json:"by_cashier"`
	Closure        *CashClosure   `json:"closure,omitempty" doc:"Présent si la journée est déjà clôturée"`
}

var cashRoles = []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleFinance}

// cashAgency : agence concernée ; un caissier ou un chef d'agence ne voit que la sienne.
func cashAgency(p auth.Principal, agencyID *uuid.UUID) (uuid.UUID, error) {
	if agencyID == nil {
		agencyID = p.AgencyID
	}
	if agencyID == nil {
		return uuid.Nil, huma.Error422UnprocessableEntity("Choisissez une agence")
	}
	if !p.HasRole(auth.RoleCompanyAdmin, auth.RoleFinance) && (p.AgencyID == nil || *p.AgencyID != *agencyID) {
		return uuid.Nil, huma.Error403Forbidden("Vous ne pouvez consulter que la caisse de votre agence")
	}
	return *agencyID, nil
}

func dayBounds(date string) (time.Time, time.Time, error) {
	d, err := time.ParseInLocation(time.DateOnly, date, beninTZ)
	if err != nil {
		return d, d, huma.Error422UnprocessableEntity("Date invalide")
	}
	return d, d.AddDate(0, 0, 1), nil
}

func (s *Service) cashSummary(ctx context.Context, q *store.Queries, companyID, agencyID uuid.UUID, date string) (CashSummary, error) {
	out := CashSummary{AgencyID: agencyID, BusinessDate: date, ByCashier: []CashierTotal{}}
	from, to, err := dayBounds(date)
	if err != nil {
		return out, err
	}
	if _, err := q.GetAgency(ctx, store.GetAgencyParams{ID: agencyID, CompanyID: companyID}); err != nil {
		if database.IsNotFound(err) {
			return out, huma.Error422UnprocessableEntity("Agence inconnue")
		}
		return out, err
	}
	t, err := q.CashTotals(ctx, store.CashTotalsParams{CompanyID: companyID, AgencyID: &agencyID, FromTs: from, ToTs: to})
	if err != nil {
		return out, err
	}
	out.CashIn, out.SalesCount, out.CashOut, out.RefundsCount = t.CashIn, t.SalesCount, t.CashOut, t.RefundsCount
	out.OnboardIn, out.HandoversCount = t.OnboardIn, t.HandoversCount
	out.ExpectedCash = t.CashIn + t.OnboardIn - t.CashOut
	rows, err := q.CashByCashier(ctx, store.CashByCashierParams{CompanyID: companyID, AgencyID: &agencyID, FromTs: from, ToTs: to})
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out.ByCashier = append(out.ByCashier, CashierTotal{StaffID: r.StaffID, FullName: r.FullName, SalesCount: r.SalesCount, Amount: r.Amount})
	}
	c, err := q.GetCashClosure(ctx, store.GetCashClosureParams{CompanyID: companyID, AgencyID: agencyID, BusinessDate: from})
	if err == nil {
		cl := CashClosure{ID: c.ID, AgencyID: c.AgencyID, BusinessDate: date, CashIn: c.CashIn, CashOut: c.CashOut,
			ExpectedCash: c.ExpectedCash, CountedCash: c.CountedCash, Difference: *c.Difference, SalesCount: c.SalesCount,
			Note: c.Note, ClosedBy: c.ClosedByName, ClosedAt: c.ClosedAt}
		out.Closure = &cl
	} else if !database.IsNotFound(err) {
		return out, err
	}
	return out, nil
}

func (s *Service) registerCash(api huma.API) {
	huma.Register(api, apix.Op("get-cash-summary", http.MethodGet, "/v1/admin/cash/summary", tagCash,
		"Caisse du jour d'une agence", apix.Staff, cashRoles...),
		func(ctx context.Context, in *struct {
			AgencyID uuid.UUID `query:"agency_id" doc:"Par défaut : agence de l'agent"`
			Date     string    `query:"date" format:"date" doc:"Par défaut : aujourd'hui (heure du Bénin)"`
		}) (*struct{ Body CashSummary }, error) {
			p := auth.MustFromContext(ctx)
			var aid *uuid.UUID
			if in.AgencyID != uuid.Nil {
				aid = &in.AgencyID
			}
			agencyID, err := cashAgency(p, aid)
			if err != nil {
				return nil, err
			}
			date := in.Date
			if date == "" {
				date = time.Now().In(beninTZ).Format(time.DateOnly)
			}
			out := &struct{ Body CashSummary }{}
			err = s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, err = s.cashSummary(ctx, q, p.CompanyID, agencyID, date)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("close-cash", http.MethodPost, "/v1/admin/cash/closures", tagCash,
		"Clôturer la caisse d'une journée", apix.Staff, auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier),
		"Le montant attendu est recalculé par le serveur au moment de la clôture. Un commentaire est obligatoire en cas d'écart. "+
			"Une seule clôture par agence et par jour, non modifiable (journal d'audit)."),
		func(ctx context.Context, in *struct {
			Body struct {
				AgencyID    *uuid.UUID `json:"agency_id,omitempty"`
				Date        string     `json:"date" format:"date"`
				CountedCash int64      `json:"counted_cash" minimum:"0" doc:"Espèces comptées dans la caisse"`
				Note        string     `json:"note,omitempty" maxLength:"1000"`
			}
		}) (*struct {
			Status int
			Body   CashClosure
		}, error) {
			p := auth.MustFromContext(ctx)
			agencyID, err := cashAgency(p, in.Body.AgencyID)
			if err != nil {
				return nil, err
			}
			from, _, err := dayBounds(in.Body.Date)
			if err != nil {
				return nil, err
			}
			if from.After(time.Now()) {
				return nil, huma.Error422UnprocessableEntity("Impossible de clôturer une journée future")
			}
			out := &struct {
				Status int
				Body   CashClosure
			}{Status: http.StatusCreated}
			err = s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				sum, err := s.cashSummary(ctx, q, p.CompanyID, agencyID, in.Body.Date)
				if err != nil {
					return err
				}
				if sum.Closure != nil {
					return huma.Error409Conflict("La caisse de cette journée est déjà clôturée")
				}
				if in.Body.CountedCash != sum.ExpectedCash && len(in.Body.Note) < 5 {
					return huma.Error422UnprocessableEntity("Écart de caisse : expliquez-le dans le commentaire")
				}
				c, err := q.InsertCashClosure(ctx, store.InsertCashClosureParams{
					CompanyID: p.CompanyID, AgencyID: agencyID, BusinessDate: from, CashIn: sum.CashIn + sum.OnboardIn, CashOut: sum.CashOut,
					ExpectedCash: sum.ExpectedCash, CountedCash: in.Body.CountedCash, SalesCount: sum.SalesCount,
					Note: in.Body.Note, ClosedBy: p.ID,
				})
				if database.UniqueViolation(err) != "" {
					return huma.Error409Conflict("La caisse de cette journée est déjà clôturée")
				}
				if err != nil {
					return err
				}
				out.Body = CashClosure{ID: c.ID, AgencyID: c.AgencyID, BusinessDate: in.Body.Date, CashIn: c.CashIn, CashOut: c.CashOut,
					ExpectedCash: c.ExpectedCash, CountedCash: c.CountedCash, Difference: *c.Difference, SalesCount: c.SalesCount,
					Note: c.Note, ClosedAt: c.ClosedAt}
				return apix.Audit(ctx, q, &p.CompanyID, "cash.closed", "agency", agencyID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-cash-closures", http.MethodGet, "/v1/admin/cash/closures", tagCash,
		"Historique des clôtures de caisse", apix.Staff, cashRoles...),
		func(ctx context.Context, in *struct {
			AgencyID uuid.UUID `query:"agency_id"`
			Limit    int32     `query:"limit" minimum:"1" maximum:"200" default:"60"`
		}) (*struct{ Body []CashClosure }, error) {
			p := auth.MustFromContext(ctx)
			var filter *uuid.UUID
			if in.AgencyID != uuid.Nil {
				filter = &in.AgencyID
			}
			if !p.HasRole(auth.RoleCompanyAdmin, auth.RoleFinance) { // caissier / chef d'agence : leur agence seulement
				if p.AgencyID == nil {
					return &struct{ Body []CashClosure }{Body: []CashClosure{}}, nil
				}
				filter = p.AgencyID
			}
			out := &struct{ Body []CashClosure }{Body: []CashClosure{}}
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCashClosures(ctx, store.ListCashClosuresParams{CompanyID: p.CompanyID, AgencyID: filter, Lim: in.Limit})
				for _, c := range rows {
					out.Body = append(out.Body, CashClosure{ID: c.ID, AgencyID: c.AgencyID, AgencyName: c.AgencyName,
						BusinessDate: c.BusinessDate.Format(time.DateOnly), CashIn: c.CashIn, CashOut: c.CashOut,
						ExpectedCash: c.ExpectedCash, CountedCash: c.CountedCash, Difference: *c.Difference, SalesCount: c.SalesCount,
						Note: c.Note, ClosedBy: c.ClosedByName, ClosedAt: c.ClosedAt})
				}
				return err
			})
			return out, apix.Err(err)
		})
}
