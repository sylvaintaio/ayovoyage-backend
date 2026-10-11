// Package seed crée un jeu de données de démonstration (développement uniquement).
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/network"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// Comptes de démonstration (à ne jamais utiliser en production).
const (
	AdminEmail   = "admin@demo.busvoyage.local"
	CashierEmail = "caisse.cotonou@demo.busvoyage.local"
	AgentEmail   = "agent@demo.busvoyage.local"
	DemoPassword = "Demo-Password-2026"
	CompanySlug  = "trans-express-demo"
)

var localTZ = time.FixedZone("WAT", 3600)

func Run(ctx context.Context, db *database.DB) error {
	return db.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		if _, err := q.GetStaffByEmail(ctx, AdminEmail); err == nil {
			slog.Info("données de démonstration déjà présentes")
			return nil
		}
		hash, err := auth.HashPassword(DemoPassword)
		if err != nil {
			return err
		}

		company, err := q.CreateCompany(ctx, store.CreateCompanyParams{
			Name: "Trans Express (démo)", Slug: CompanySlug,
			LegalName: ptr("Trans Express Démo SARL"), Phone: ptr("+22901000000"), Email: ptr("contact@demo.busvoyage.local"),
		})
		if err != nil {
			return err
		}

		cities := map[string]store.City{}
		for _, c := range []struct{ name, slug string }{
			{"Cotonou", "cotonou"}, {"Parakou", "parakou"}, {"Porto-Novo", "porto-novo"}, {"Bohicon", "bohicon"},
			{"Natitingou", "natitingou"}, {"Djougou", "djougou"}, {"Abomey-Calavi", "abomey-calavi"},
			{"Dassa-Zoumè", "dassa-zoume"}, {"Savè", "save"},
		} {
			city, err := q.CreateCity(ctx, store.CreateCityParams{Name: c.name, Slug: c.slug, CountryCode: "BJ"})
			if err != nil {
				return err
			}
			cities[c.slug] = city
		}

		hours, _ := json.Marshal([]map[string]string{
			{"day": "mon", "open": "05:30", "close": "21:00"}, {"day": "tue", "open": "05:30", "close": "21:00"},
			{"day": "wed", "open": "05:30", "close": "21:00"}, {"day": "thu", "open": "05:30", "close": "21:00"},
			{"day": "fri", "open": "05:30", "close": "21:00"}, {"day": "sat", "open": "05:30", "close": "21:00"},
			{"day": "sun", "open": "06:00", "close": "19:00"},
		})
		agencies := map[string]store.Agency{}
		for _, a := range []struct {
			key, city, name, address, landmark string
			lat, lng                           float64
			services                           []string
		}{
			{"cotonou", "cotonou", "Cotonou — Jonquet", "Rue 112, Jonquet", "Face à la pharmacie Jonquet", 6.3654, 2.4183, []string{"ticketing", "parcel_drop", "parcel_pickup", "lounge"}},
			{"calavi", "abomey-calavi", "Abomey-Calavi — Carrefour IITA", "Route de l'IITA", "À 200 m du carrefour IITA", 6.4485, 2.3557, []string{"ticketing", "parcel_drop"}},
			{"bohicon", "bohicon", "Bohicon — Gare routière", "Gare routière de Bohicon", "Entrée principale", 7.1782, 2.0667, []string{"ticketing", "parcel_drop", "parcel_pickup"}},
			{"parakou", "parakou", "Parakou — Centre", "Avenue de la Paix", "Près du marché Arzèkè", 9.3372, 2.6303, []string{"ticketing", "parcel_drop", "parcel_pickup", "lounge"}},
			{"natitingou", "natitingou", "Natitingou — Centre", "Route nationale 3", "Station-service centrale", 10.3042, 1.3796, []string{"ticketing", "parcel_pickup"}},
		} {
			ag, err := q.CreateAgency(ctx, store.CreateAgencyParams{
				CompanyID: company.ID, CityID: cities[a.city].ID, Name: a.name, Slug: CompanySlug + "-" + a.key,
				Address: a.address, Landmark: a.landmark, Latitude: &a.lat, Longitude: &a.lng,
				Phone: "+22901000001", Whatsapp: "+22901000001", Email: a.key + "@demo.busvoyage.local",
				OpeningHours: hours, Services: a.services, Photos: []string{}, IsActive: true,
				Instructions: "Présentez-vous 30 minutes avant le départ avec votre QR code. Bagage en soute : 20 kg inclus.",
			})
			if err != nil {
				return err
			}
			agencies[a.key] = ag
			if err := network.EnsureAgencyStopPoint(ctx, q, ag); err != nil {
				return err
			}
		}

		// Points d'arrêt : ceux des agences (créés ci-dessus) et des arrêts en bord de route.
		stops := map[string]uuid.UUID{}
		for key, ag := range agencies {
			sp, err := q.GetStopPointByAgency(ctx, &ag.ID)
			if err != nil {
				return err
			}
			stops[key] = sp.ID
		}
		for _, sp := range []struct{ key, city, name, address string }{
			{"dassa", "dassa-zoume", "Carrefour Dassa", "Carrefour de la RNIE 2, devant la station"},
			{"save", "save", "Station Savè", "Station-service à l'entrée nord de Savè"},
			{"parakou-gare", "parakou", "Gare routière de Parakou", "Gare routière centrale"},
		} {
			created, err := q.CreateStopPoint(ctx, store.CreateStopPointParams{CompanyID: company.ID, CityID: cities[sp.city].ID,
				Name: sp.name, Address: sp.address, IsActive: true})
			if err != nil {
				return err
			}
			stops[sp.key] = created.ID
		}

		// Catégories : 4 VIP à l'avant, 8 Premium, le reste Standard (exemple du cahier de conception).
		std, err := q.CreateSeatCategory(ctx, store.CreateSeatCategoryParams{CompanyID: company.ID, Code: "standard", Name: "Standard",
			Description: "Tarif de base", Color: "#7d8681", Perks: []string{}, SortOrder: 3})
		if err != nil {
			return err
		}
		prem, err := q.CreateSeatCategory(ctx, store.CreateSeatCategoryParams{CompanyID: company.ID, Code: "premium", Name: "Premium",
			Description: "Meilleur emplacement, plus d'espace", SurchargeAmount: 2000, Color: "#2f8a68",
			Perks: []string{"Siège à l'avant", "Espace pour les jambes"}, SortOrder: 2})
		if err != nil {
			return err
		}
		vip, err := q.CreateSeatCategory(ctx, store.CreateSeatCategoryParams{CompanyID: company.ID, Code: "vip", Name: "VIP",
			Description: "Expérience premium", SurchargeAmount: 5000, Color: "#c98a00",
			Perks: []string{"Embarquement prioritaire", "Bagage supplémentaire", "Eau offerte", "Accès lounge"}, SortOrder: 1})
		if err != nil {
			return err
		}

		// Plan 2+2 : 13 rangées × 5 colonnes (colonne 2 = allée), poste chauffeur devant.
		const rows, cols = 13, 5
		layout, err := q.CreateSeatLayout(ctx, store.CreateSeatLayoutParams{CompanyID: company.ID, Name: "Autocar 2+2 — 48 places", Rows: rows, Cols: cols})
		if err != nil {
			return err
		}
		letters := []string{"A", "B", "", "C", "D"}
		seats := int32(0)
		for r := int32(0); r < rows; r++ {
			for c := int32(0); c < cols; c++ {
				p := store.InsertSeatLayoutCellParams{CompanyID: company.ID, LayoutID: layout.ID, RowIndex: r, ColIndex: c, Attributes: []string{}}
				switch {
				case r == 0 && c == 0:
					p.Kind = "driver"
				case r == 0 && c == 4:
					p.Kind = "door"
				case r == 0:
					continue
				case c == 2:
					p.Kind = "aisle"
				default:
					label := fmt.Sprintf("%d%s", r, letters[c])
					p.Kind, p.SeatLabel = "seat", &label
					cat := std.ID
					switch {
					case r == 1:
						cat = vip.ID
					case r <= 3:
						cat = prem.ID
					}
					p.CategoryID = &cat
					if c == 0 || c == 4 {
						p.Attributes = []string{"window"}
					} else {
						p.Attributes = []string{"aisle"}
					}
					if r == 1 {
						p.Attributes = append(p.Attributes, "front", "legroom")
					}
					seats++
				}
				if err := q.InsertSeatLayoutCell(ctx, p); err != nil {
					return err
				}
			}
		}
		if _, err := q.UpdateSeatLayout(ctx, store.UpdateSeatLayoutParams{ID: layout.ID, CompanyID: company.ID,
			Name: fmt.Sprintf("Autocar 2+2 — %d places", seats), Rows: rows, Cols: cols, SeatCount: seats}); err != nil {
			return err
		}

		var buses []store.Bus
		for i, plate := range []string{"AB 1234 RB", "CD 5678 RB", "EF 9012 RB"} {
			b, err := q.CreateBus(ctx, store.CreateBusParams{CompanyID: company.ID, PlateNumber: plate,
				InternalCode: fmt.Sprintf("BUS-%02d", i+1), Model: "Yutong ZK6122", LayoutID: layout.ID,
				Amenities: []string{"climatisation", "usb", "wifi"}, Status: "active", HomeAgencyID: ptrUUID(agencies["cotonou"].ID)})
			if err != nil {
				return err
			}
			buses = append(buses, b)
		}

		// Lignes à arrêts. Cotonou → Parakou : grille de prix (on paie selon la descente).
		type seedStop struct {
			key           string
			offset, pause int32
			board, alight bool
		}
		newRoute := func(name string, price int64, distance int32, mode string, stopsIn []seedStop, fares map[[2]string]int64) (store.Route, error) {
			first := agencies[stopsIn[0].key]
			rt, err := q.CreateRoute(ctx, store.CreateRouteParams{CompanyID: company.ID, OriginAgencyID: first.ID, Name: name,
				BasePrice: price, DurationMinutes: 1, DistanceKm: ptrInt(distance), IncludedBaggageKg: 20, IsActive: true})
			if err != nil {
				return rt, err
			}
			for i, st := range stopsIn {
				if err := q.InsertRouteStop(ctx, store.InsertRouteStopParams{RouteID: rt.ID, CompanyID: company.ID, Seq: int32(i),
					StopPointID: stops[st.key], OffsetMinutes: st.offset, PauseMinutes: st.pause, CanBoard: st.board, CanAlight: st.alight}); err != nil {
					return rt, err
				}
			}
			for pair, amount := range fares {
				if err := q.InsertRouteFare(ctx, store.InsertRouteFareParams{RouteID: rt.ID, CompanyID: company.ID,
					FromCityID: cities[pair[0]].ID, ToCityID: cities[pair[1]].ID, Price: amount}); err != nil {
					return rt, err
				}
			}
			last := stopsIn[len(stopsIn)-1]
			var dest *uuid.UUID
			if ag, ok := agencies[last.key]; ok {
				dest = &ag.ID
			}
			return q.SetRouteItinerary(ctx, store.SetRouteItineraryParams{ID: rt.ID, CompanyID: company.ID, OriginAgencyID: first.ID,
				DestinationAgencyID: dest, DurationMinutes: last.offset, PricingMode: mode})
		}
		cotParakou, err := newRoute("Cotonou → Parakou", 7500, 415, "grid", []seedStop{
			{key: "cotonou", board: true},
			{key: "calavi", offset: 40, board: true, alight: true},
			{key: "bohicon", offset: 150, pause: 15, board: true, alight: true},
			{key: "dassa", offset: 240, alight: true},
			{key: "save", offset: 300, alight: true},
			{key: "parakou", offset: 420, alight: true},
		}, map[[2]string]int64{
			{"cotonou", "abomey-calavi"}: 1500, {"cotonou", "bohicon"}: 3500, {"cotonou", "dassa-zoume"}: 5000, {"cotonou", "save"}: 6000,
			{"abomey-calavi", "bohicon"}: 3000, {"abomey-calavi", "dassa-zoume"}: 4500, {"abomey-calavi", "save"}: 5500,
			{"bohicon", "dassa-zoume"}: 2000, {"bohicon", "save"}: 3000, {"bohicon", "parakou"}: 5000,
		})
		if err != nil {
			return err
		}
		parakouCot, err := newRoute("Parakou → Cotonou", 7500, 415, "flat", []seedStop{
			{key: "parakou", board: true},
			{key: "bohicon", offset: 270, pause: 15, board: true, alight: true},
			{key: "cotonou", offset: 420, alight: true},
		}, nil)
		if err != nil {
			return err
		}
		cotNati, err := newRoute("Cotonou → Natitingou", 10000, 530, "flat", []seedStop{
			{key: "cotonou", board: true},
			{key: "bohicon", offset: 150, pause: 15, board: true, alight: true},
			{key: "natitingou", offset: 600, alight: true},
		}, nil)
		if err != nil {
			return err
		}

		admin, err := q.CreateStaff(ctx, store.CreateStaffParams{CompanyID: company.ID, Email: AdminEmail, PasswordHash: hash,
			FullName: "Administrateur Démo", Phone: "+22901000010", Role: auth.RoleCompanyAdmin})
		if err != nil {
			return err
		}
		if _, err := q.CreateStaff(ctx, store.CreateStaffParams{CompanyID: company.ID, AgencyID: ptrUUID(agencies["cotonou"].ID),
			Email: CashierEmail, PasswordHash: hash, FullName: "Caissier Cotonou", Phone: "+22901000011", Role: auth.RoleCashier}); err != nil {
			return err
		}
		agent, err := q.CreateStaff(ctx, store.CreateStaffParams{CompanyID: company.ID, AgencyID: ptrUUID(agencies["cotonou"].ID),
			Email: AgentEmail, PasswordHash: hash, FullName: "Convoyeur Démo", Phone: "+22901000012", Role: auth.RoleBoardingAgent})
		if err != nil {
			return err
		}
		if _, err := q.CreateDevice(ctx, store.CreateDeviceParams{CompanyID: company.ID, StaffID: agent.ID, Name: "Terminal démo", Platform: "android"}); err != nil {
			return err
		}

		// Départs sur 7 jours : Cotonou → Parakou à 7h et 14h, retour à 7h, Natitingou à 6h.
		today := time.Now().In(localTZ)
		base := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, localTZ)
		for d := 0; d < 7; d++ {
			day := base.AddDate(0, 0, d)
			for _, s := range []struct {
				route store.Route
				bus   store.Bus
				hour  int
			}{
				{cotParakou, buses[0], 7}, {parakouCot, buses[1], 7}, {cotParakou, buses[1], 22}, {cotNati, buses[2], 6},
			} {
				dep := day.Add(time.Duration(s.hour) * time.Hour)
				if dep.Before(time.Now().Add(30 * time.Minute)) {
					continue
				}
				created, err := q.CreateDeparture(ctx, store.CreateDepartureParams{CompanyID: company.ID, RouteID: s.route.ID, BusID: s.bus.ID,
					LayoutID: layout.ID, DepartureAt: dep, ArrivalEta: dep.Add(time.Duration(s.route.DurationMinutes) * time.Minute),
					BasePrice: s.route.BasePrice, AgentID: &agent.ID})
				if err != nil {
					return err
				}
				if err := q.CopyRouteStopsToDeparture(ctx, store.CopyRouteStopsToDepartureParams{
					DepartureID: created.ID, DepartureAt: dep, RouteID: s.route.ID}); err != nil {
					return err
				}
			}
		}

		// Tarifs colis.
		for _, t := range []store.UpsertParcelTariffParams{
			{Category: "document", Label: "Document / enveloppe", BasePrice: 1500, IncludedKg: 1, PricePerKg: 0, MaxWeightKg: 1, InsurancePercent: 1},
			{Category: "small", Label: "Petit colis", BasePrice: 2500, IncludedKg: 5, PricePerKg: 300, MaxWeightKg: 10, InsurancePercent: 1},
			{Category: "medium", Label: "Colis moyen", BasePrice: 4000, IncludedKg: 15, PricePerKg: 250, MaxWeightKg: 30, InsurancePercent: 1},
			{Category: "large", Label: "Gros colis", BasePrice: 7000, IncludedKg: 30, PricePerKg: 200, MaxWeightKg: 80, InsurancePercent: 1},
			{Category: "fragile", Label: "Fragile", BasePrice: 5000, IncludedKg: 10, PricePerKg: 400, MaxWeightKg: 30, InsurancePercent: 1},
		} {
			t.CompanyID = company.ID
			if _, err := q.UpsertParcelTariff(ctx, t); err != nil {
				return err
			}
		}

		// Programme de fidélité.
		if _, err := q.UpsertLoyaltyProgram(ctx, store.UpsertLoyaltyProgramParams{CompanyID: company.ID, IsEnabled: true,
			PointsPer100: 1, PointValue: 5, MaxRedeemPercent: 50, PointsValidityMonths: 12, SignupBonusPoints: 50,
			RefereeRewardAmount: 1000, ReferrerRewardPoints: 200, ReferrerMonthlyCap: 10}); err != nil {
			return err
		}
		for i, t := range []store.InsertLoyaltyTierParams{
			{Code: "bronze", Name: "Bronze", MinTrips: 0, Perks: []string{"Cumul de points", "Offre anniversaire"}},
			{Code: "silver", Name: "Argent", MinTrips: 6, DiscountPercent: 5, Perks: []string{"Réservation anticipée", "Annulation jusqu'à 3 h avant, frais réduits à 5 %"},
				CancellationFeePercent: ptrInt(5), CancellationCutoffHours: ptrInt(3)},
			{Code: "gold", Name: "Or", MinTrips: 15, DiscountPercent: 10, Perks: []string{"Modification de billet gratuite", "Annulation jusqu'à 3 h avant, frais réduits à 5 %", "Bagage supplémentaire gratuit", "Embarquement prioritaire"},
				CancellationFeePercent: ptrInt(5), CancellationCutoffHours: ptrInt(3), FreeChange: true},
			{Code: "platinum", Name: "Platine", MinTrips: 30, DiscountPercent: 15, Perks: []string{"Annulation gratuite jusqu'à 1 h avant", "Modification de billet gratuite", "Surclassement si disponible", "Support prioritaire"},
				CancellationFeePercent: ptrInt(0), CancellationCutoffHours: ptrInt(1), FreeChange: true},
		} {
			t.CompanyID, t.SortOrder = company.ID, int32(i)
			if _, err := q.InsertLoyaltyTier(ctx, t); err != nil {
				return err
			}
		}

		slog.Info("données de démonstration créées",
			"compagnie", company.Name, "admin", admin.Email, "caissier", CashierEmail, "agent", AgentEmail,
			"mot_de_passe", DemoPassword, "sièges_par_bus", seats)
		return nil
	})
}

func ptr(s string) *string            { return &s }
func ptrInt(i int32) *int32           { return &i }
func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }
