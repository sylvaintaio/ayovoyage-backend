// Package parcel gère les colis (dépôt, transport, correspondances, remise
// sécurisée), les bagages des voyageurs et les réclamations.
//
// Cycle d'un colis :
//
//	accepted → assigned → loaded → in_transit → ready → delivered
//	                                         ↘ arrived (correspondance) → assigned → …
//
// Chaque étape est tracée dans parcel_events (append-only). Le passage en
// transit et l'arrivée sont déclenchés par le statut du départ (départ parti /
// arrivé) ; un agent peut aussi réceptionner un colis par scan.
package parcel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/config"
	"busvoyage/internal/invoice"
	"busvoyage/internal/notify"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/phone"
	"busvoyage/internal/platform/refgen"
	"busvoyage/internal/segment"
	"busvoyage/internal/store"
)

type Service struct {
	DB    *database.DB
	Cfg   config.Config
	SMS   notify.SMSSender
	Email notify.EmailSender
}

var localTZ = time.FixedZone("WAT", 3600)

// Rôles autorisés.
var (
	parcelRoles = []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleParcelAgent}
	loadRoles   = []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleParcelAgent,
		auth.RoleBoardingAgent, auth.RoleDriver}
)

var statusLabels = map[string]string{
	"accepted":   "Déposé en agence",
	"assigned":   "Affecté à un départ",
	"loaded":     "Chargé dans le bus",
	"in_transit": "En route",
	"arrived":    "Arrivé en agence de correspondance",
	"ready":      "Disponible au retrait",
	"delivered":  "Remis au destinataire",
	"returned":   "Rendu à l'expéditeur",
	"cancelled":  "Annulé",
}

// ---------------------------------------------------------------- Prix

type ParcelQuote struct {
	Category   string  `json:"category"`
	Label      string  `json:"label"`
	BasePrice  int64   `json:"base_price"`
	WeightFee  int64   `json:"weight_fee" doc:"Supplément au-delà du poids inclus"`
	Insurance  int64   `json:"insurance" doc:"Assurance sur la valeur déclarée"`
	Total      int64   `json:"total"`
	MaxWeight  float64 `json:"max_weight_kg"`
	IncludedKg float64 `json:"included_kg"`
}

// price : prix de base + kilos au-delà du poids inclus (arrondis au kilo
// supérieur) + assurance en pourcentage de la valeur déclarée ; arrondi à 25 F.
func price(t store.ParcelTariff, weight float64, declared int64) (ParcelQuote, error) {
	q := ParcelQuote{Category: t.Category, Label: t.Label, BasePrice: t.BasePrice, MaxWeight: t.MaxWeightKg, IncludedKg: t.IncludedKg}
	if weight <= 0 {
		return q, huma.Error422UnprocessableEntity("Indiquez le poids du colis")
	}
	if weight > t.MaxWeightKg {
		return q, huma.Error422UnprocessableEntity(fmt.Sprintf("%s : %g kg au plus. Choisissez une catégorie plus grande.", t.Label, t.MaxWeightKg))
	}
	if extra := weight - t.IncludedKg; extra > 0 {
		q.WeightFee = int64(math.Ceil(extra)) * t.PricePerKg
	}
	q.Insurance = int64(math.Ceil(float64(declared) * t.InsurancePercent / 100))
	q.Total = roundUp(q.BasePrice+q.WeightFee+q.Insurance, 25)
	return q, nil
}

func roundUp(v, step int64) int64 {
	if r := v % step; r != 0 {
		return v + step - r
	}
	return v
}

// ---------------------------------------------------------------- Création

type ParcelInput struct {
	SenderName          string     `json:"sender_name" minLength:"2" maxLength:"120"`
	SenderPhone         string     `json:"sender_phone" minLength:"8" maxLength:"20"`
	SenderEmail         string     `json:"sender_email,omitempty" format:"email" maxLength:"254"`
	RecipientName       string     `json:"recipient_name" minLength:"2" maxLength:"120"`
	RecipientPhone      string     `json:"recipient_phone" minLength:"8" maxLength:"20"`
	RecipientEmail      string     `json:"recipient_email,omitempty" format:"email" maxLength:"254"`
	OriginAgencyID      *uuid.UUID `json:"origin_agency_id,omitempty" doc:"Agence de dépôt (par défaut celle de l'agent)"`
	DestinationAgencyID uuid.UUID  `json:"destination_agency_id"`
	Category            string     `json:"category" enum:"document,small,medium,large,fragile"`
	Description         string     `json:"description" minLength:"2" maxLength:"300" doc:"Contenu déclaré"`
	WeightKg            float64    `json:"weight_kg" minimum:"0.01" maximum:"500"`
	Pieces              int32      `json:"pieces,omitempty" minimum:"1" maximum:"50" default:"1"`
	DeclaredValue       int64      `json:"declared_value,omitempty" minimum:"0" maximum:"100000000"`
	Payer               string     `json:"payer" enum:"sender,recipient" doc:"sender : payé au dépôt ; recipient : payé au retrait (port dû)"`
	Photo               string     `json:"photo,omitempty" maxLength:"2900000" doc:"Photo au dépôt (data URL JPEG/PNG, 2 Mo au plus)"`
}

func (s *Service) normPhone(raw, label string) (string, error) {
	p, err := phone.Normalize(raw, s.Cfg.DefaultCountryCode)
	if err != nil {
		return "", huma.Error422UnprocessableEntity("Téléphone invalide : " + label)
	}
	return p, nil
}

// staffAgency : agence de l'agent, ou celle choisie par un administrateur.
func staffAgency(p auth.Principal, requested *uuid.UUID) (uuid.UUID, error) {
	if requested != nil && (p.AgencyID == nil || p.Role == auth.RoleCompanyAdmin) {
		return *requested, nil
	}
	if p.AgencyID == nil {
		return uuid.Nil, huma.Error422UnprocessableEntity("Choisissez l'agence")
	}
	if requested != nil && *requested != *p.AgencyID {
		return uuid.Nil, huma.Error403Forbidden("Vous ne pouvez agir que pour votre agence")
	}
	return *p.AgencyID, nil
}

func hasService(a store.Agency, svc string) bool {
	for _, x := range a.Services {
		if x == svc {
			return true
		}
	}
	return false
}

// Create enregistre un colis déposé au guichet ; s'il est payé au dépôt,
// l'encaissement en espèces entre dans la caisse de l'agence.
func (s *Service) Create(ctx context.Context, p auth.Principal, in ParcelInput) (uuid.UUID, error) {
	senderPhone, err := s.normPhone(in.SenderPhone, "expéditeur")
	if err != nil {
		return uuid.Nil, err
	}
	recipientPhone, err := s.normPhone(in.RecipientPhone, "destinataire")
	if err != nil {
		return uuid.Nil, err
	}
	originID, err := staffAgency(p, in.OriginAgencyID)
	if err != nil {
		return uuid.Nil, err
	}
	if in.Photo != "" && !strings.HasPrefix(in.Photo, "data:image/") {
		return uuid.Nil, huma.Error422UnprocessableEntity("Photo invalide")
	}
	if in.Pieces == 0 {
		in.Pieces = 1
	}
	var id uuid.UUID
	err = s.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
		origin, err := q.GetAgency(ctx, store.GetAgencyParams{ID: originID, CompanyID: p.CompanyID})
		if err != nil {
			return huma.Error422UnprocessableEntity("Agence de dépôt inconnue")
		}
		dest, err := q.GetAgency(ctx, store.GetAgencyParams{ID: in.DestinationAgencyID, CompanyID: p.CompanyID})
		if err != nil {
			return huma.Error422UnprocessableEntity("Agence de destination inconnue")
		}
		switch {
		case origin.ID == dest.ID:
			return huma.Error422UnprocessableEntity("L'agence de destination doit être différente de l'agence de dépôt")
		case !hasService(origin, "parcel_drop") || !origin.IsActive:
			return huma.Error422UnprocessableEntity(origin.Name + " ne reçoit pas de colis")
		case !hasService(dest, "parcel_pickup") || !dest.IsActive:
			return huma.Error422UnprocessableEntity(dest.Name + " ne fait pas le retrait de colis")
		}
		t, err := q.GetParcelTariff(ctx, store.GetParcelTariffParams{CompanyID: p.CompanyID, Category: in.Category})
		if err != nil {
			return huma.Error422UnprocessableEntity("Catégorie non proposée par la compagnie")
		}
		quote, err := price(t, in.WeightKg, in.DeclaredValue)
		if err != nil {
			return err
		}
		var paidAt *time.Time
		if in.Payer == "sender" {
			now := time.Now()
			paidAt = &now
		}
		staffID := p.ID
		parcel, err := q.InsertParcel(ctx, store.InsertParcelParams{
			CompanyID: p.CompanyID, TrackingNumber: refgen.Parcel(),
			SenderName: strings.TrimSpace(in.SenderName), SenderPhone: senderPhone, SenderEmail: apix.NilIfEmpty(in.SenderEmail),
			SenderCustomerID: s.customerID(ctx, q, senderPhone, in.SenderEmail),
			RecipientName:    strings.TrimSpace(in.RecipientName), RecipientPhone: recipientPhone, RecipientEmail: apix.NilIfEmpty(in.RecipientEmail),
			RecipientCustomerID: s.customerID(ctx, q, recipientPhone, in.RecipientEmail),
			OriginAgencyID:      origin.ID, DestinationAgencyID: dest.ID, Category: in.Category, Description: strings.TrimSpace(in.Description),
			WeightKg: in.WeightKg, DeclaredValue: in.DeclaredValue, Pieces: in.Pieces, Price: quote.Total, Currency: "XOF",
			Payer: in.Payer, PaidAt: paidAt, Photo: apix.NilIfEmpty(in.Photo), CreatedBy: &staffID,
		})
		if err != nil {
			return err
		}
		id = parcel.ID
		if paidAt != nil && parcel.Price > 0 {
			if err := s.cashPayment(ctx, tx, q, p, parcel, origin.ID); err != nil {
				return err
			}
		}
		if err := s.event(ctx, q, parcel, "accepted", &origin.ID, nil, &staffID, ""); err != nil {
			return err
		}
		if err := apix.Audit(ctx, q, &p.CompanyID, "parcel.created", "parcel", parcel.ID.String(), nil,
			map[string]any{"tracking": parcel.TrackingNumber, "price": parcel.Price, "payer": parcel.Payer}); err != nil {
			return err
		}
		link := s.trackingLink(parcel.TrackingNumber)
		s.notify(ctx, q, parcel, "sender", "parcel_created",
			fmt.Sprintf("Colis %s enregistré pour %s (%s). Suivi : %s", parcel.TrackingNumber, parcel.RecipientName, dest.Name, link))
		s.notify(ctx, q, parcel, "recipient", "parcel_incoming",
			fmt.Sprintf("%s vous envoie un colis (%s), à retirer à %s. Vous serez prévenu à son arrivée. Suivi : %s",
				parcel.SenderName, parcel.TrackingNumber, dest.Name, link))
		return nil
	})
	return id, err
}

func (s *Service) customerID(ctx context.Context, q *store.Queries, phoneNumber, email string) *uuid.UUID {
	id, err := q.FindCustomerByPhoneOrEmail(ctx, store.FindCustomerByPhoneOrEmailParams{Phone: &phoneNumber, Email: apix.NilIfEmpty(email)})
	if err != nil {
		return nil
	}
	return &id
}

func (s *Service) cashPayment(ctx context.Context, tx pgx.Tx, q *store.Queries, p auth.Principal, parcel store.Parcel, agencyID uuid.UUID) error {
	staffID := p.ID
	pay, err := q.InsertPayment(ctx, store.InsertPaymentParams{
		CompanyID: parcel.CompanyID, ParcelID: &parcel.ID, Provider: "cash", Method: "cash", Amount: parcel.Price,
		Currency: parcel.Currency, PayerPhone: parcel.SenderPhone, IdempotencyKey: "cash:parcel:" + parcel.ID.String(),
		AgencyID: &agencyID, ReceivedByStaffID: &staffID,
	})
	if err != nil {
		return err
	}
	paid, err := q.MarkPaymentSucceeded(ctx, pay.ID)
	if err != nil {
		return err
	}
	invoice.ForParcel(ctx, tx, q, paid, parcel)
	return nil
}

func (s *Service) event(ctx context.Context, q *store.Queries, parcel store.Parcel, typ string, agencyID, departureID, staffID *uuid.UUID, note string) error {
	return q.InsertParcelEvent(ctx, store.InsertParcelEventParams{
		CompanyID: parcel.CompanyID, ParcelID: parcel.ID, EventType: typ, StatusAfter: parcel.Status,
		AgencyID: agencyID, DepartureID: departureID, StaffID: staffID, Note: note,
	})
}

func (s *Service) trackingLink(tracking string) string {
	return s.Cfg.PublicWebURL + "/colis/suivi/" + tracking
}

// notify envoie un SMS (et un e-mail si l'adresse est connue) à l'expéditeur
// ou au destinataire, et l'enregistre dans l'historique des messages.
func (s *Service) notify(ctx context.Context, q *store.Queries, parcel store.Parcel, to, template, text string) {
	phoneNumber, email, customerID := parcel.SenderPhone, parcel.SenderEmail, parcel.SenderCustomerID
	if to == "recipient" {
		phoneNumber, email, customerID = parcel.RecipientPhone, parcel.RecipientEmail, parcel.RecipientCustomerID
	}
	company, _ := q.GetCompany(ctx, parcel.CompanyID)
	sms := company.Name + " : " + text
	record := func(channel, recipient, subject, body string, err error) {
		sender := any(s.SMS)
		if channel == "email" {
			sender = s.Email
		}
		status, msg := notify.DeliveryStatus(sender, err)
		if err != nil {
			slog.Error("notification colis", "parcel", parcel.TrackingNumber, "channel", channel, "err", err)
		}
		_ = q.InsertNotification(ctx, store.InsertNotificationParams{CompanyID: &parcel.CompanyID, CustomerID: customerID,
			Channel: channel, Template: template, Recipient: recipient, Status: status, Error: msg, Subject: subject, Body: body})
	}
	record("sms", phoneNumber, "", sms, s.SMS.SendSMS(ctx, phoneNumber, sms))
	if email != nil && *email != "" && s.Email != nil {
		brand := notify.BrandFromConfig(s.Cfg)
		brand.Name = company.Name
		msg := notify.ParcelEmail(brand, *email, "Colis "+parcel.TrackingNumber, text, s.trackingLink(parcel.TrackingNumber))
		record("email", *email, msg.Subject, msg.Text, s.Email.SendEmail(ctx, msg))
	}
}

// ---------------------------------------------------------------- Transitions

func loadParcel(ctx context.Context, q *store.Queries, p auth.Principal, id uuid.UUID) (store.Parcel, error) {
	parcel, err := q.GetParcelForUpdate(ctx, id)
	if database.IsNotFound(err) || (err == nil && parcel.CompanyID != p.CompanyID) {
		return parcel, apix.NotFound("Colis")
	}
	return parcel, err
}

// Assign affecte le colis à un départ partant de l'agence où il se trouve.
func (s *Service) Assign(ctx context.Context, p auth.Principal, id, departureID uuid.UUID) error {
	return s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		parcel, err := loadParcel(ctx, q, p, id)
		if err != nil {
			return err
		}
		if parcel.Status != "accepted" && parcel.Status != "arrived" && parcel.Status != "assigned" {
			return huma.Error409Conflict("Ce colis ne peut pas être affecté (" + statusLabels[parcel.Status] + ")")
		}
		dep, err := q.GetDepartureRouteAgencies(ctx, departureID)
		if err != nil || dep.CompanyID != p.CompanyID {
			return apix.NotFound("Départ")
		}
		// Le car doit s'arrêter à l'agence où se trouve le colis, et ne pas l'avoir encore quittée.
		if parcel.CurrentAgencyID == nil {
			return huma.Error422UnprocessableEntity("Ce colis n'est dans aucune agence")
		}
		loadSeq, err := q.DepartureStopAtAgency(ctx, store.DepartureStopAtAgencyParams{DepartureID: dep.ID, AgencyID: parcel.CurrentAgencyID})
		if database.IsNotFound(err) {
			return huma.Error422UnprocessableEntity("Ce départ ne s'arrête pas à l'agence où se trouve le colis")
		}
		if err != nil {
			return err
		}
		if err := segment.OpenForSale(ctx, q, store.Departure{ID: dep.ID, Status: dep.Status}, int(loadSeq)); err != nil {
			return huma.Error409Conflict("Ce départ n'accepte plus de chargement à cette agence")
		}
		parcel, err = q.SetParcelState(ctx, store.SetParcelStateParams{ID: parcel.ID, Status: "assigned",
			CurrentAgencyID: parcel.CurrentAgencyID, DepartureID: &dep.ID})
		if err != nil {
			return err
		}
		note := "Correspondance"
		if _, err := q.DepartureStopAtAgency(ctx, store.DepartureStopAtAgencyParams{DepartureID: dep.ID,
			AgencyID: &parcel.DestinationAgencyID}); err == nil {
			note = "Trajet direct"
		}
		staffID := p.ID
		return s.event(ctx, q, parcel, "assigned", parcel.CurrentAgencyID, &dep.ID, &staffID,
			note+", départ du "+dep.DepartureAt.In(localTZ).Format("02/01 à 15h04"))
	})
}

// Unassign retire le colis de son départ (il reste en agence).
func (s *Service) Unassign(ctx context.Context, p auth.Principal, id uuid.UUID, reason string) error {
	return s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		parcel, err := loadParcel(ctx, q, p, id)
		if err != nil {
			return err
		}
		if parcel.Status != "assigned" && parcel.Status != "loaded" {
			return huma.Error409Conflict("Ce colis n'est affecté à aucun départ en préparation")
		}
		return s.backToAgency(ctx, q, parcel, &p.ID, "unassigned", reason)
	})
}

// backToAgency : colis non parti, de nouveau en attente dans son agence.
func (s *Service) backToAgency(ctx context.Context, q *store.Queries, parcel store.Parcel, staffID *uuid.UUID, typ, note string) error {
	status := "arrived"
	if parcel.CurrentAgencyID != nil && *parcel.CurrentAgencyID == parcel.OriginAgencyID {
		status = "accepted"
	}
	dep := parcel.DepartureID
	parcel, err := q.SetParcelState(ctx, store.SetParcelStateParams{ID: parcel.ID, Status: status,
		CurrentAgencyID: parcel.CurrentAgencyID, DepartureID: nil})
	if err != nil {
		return err
	}
	return s.event(ctx, q, parcel, typ, parcel.CurrentAgencyID, dep, staffID, note)
}

// Load : scan au chargement dans le bus du départ affecté.
func (s *Service) Load(ctx context.Context, p auth.Principal, tracking string, departureID uuid.UUID) (store.Parcel, error) {
	var parcel store.Parcel
	err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		var err error
		parcel, err = q.GetParcelByTrackingForUpdate(ctx, tracking)
		if database.IsNotFound(err) || (err == nil && parcel.CompanyID != p.CompanyID) {
			return apix.NotFound("Colis")
		}
		if err != nil {
			return err
		}
		switch {
		case parcel.Status == "loaded" && parcel.DepartureID != nil && *parcel.DepartureID == departureID:
			return nil // déjà chargé : scan répété sans effet
		case parcel.Status != "assigned":
			return huma.Error409Conflict("Colis non prévu au chargement (" + statusLabels[parcel.Status] + ")")
		case parcel.DepartureID == nil || *parcel.DepartureID != departureID:
			return huma.Error409Conflict("Ce colis est affecté à un autre départ : ne pas le charger")
		}
		if parcel, err = q.SetParcelState(ctx, store.SetParcelStateParams{ID: parcel.ID, Status: "loaded",
			CurrentAgencyID: parcel.CurrentAgencyID, DepartureID: parcel.DepartureID}); err != nil {
			return err
		}
		staffID := p.ID
		return s.event(ctx, q, parcel, "loaded", parcel.CurrentAgencyID, parcel.DepartureID, &staffID, "")
	})
	return parcel, err
}

// OnDepartureStatus est appelé dans la transaction qui change le statut d'un départ.
func (s *Service) OnDepartureStatus(ctx context.Context, q *store.Queries, departureID uuid.UUID, status string) error {
	parcels, err := q.ListParcelsOnDeparture(ctx, &departureID)
	if err != nil || len(parcels) == 0 {
		return err
	}
	dep, err := q.GetDepartureRouteAgencies(ctx, departureID)
	if err != nil {
		return err
	}
	for _, parcel := range parcels {
		switch status {
		case "departed":
			// Départ du premier arrêt : seuls les colis de cette agence partent. Ceux à charger plus loin
			// (Bohicon…) suivent le journal des arrêts (OnStopDeparture).
			if parcel.CurrentAgencyID == nil || *parcel.CurrentAgencyID != dep.OriginAgencyID {
				continue
			}
			if parcel.Status == "assigned" { // pas chargé : reste en agence
				if err := s.backToAgency(ctx, q, parcel, nil, "left_behind", "Non chargé au départ du bus : à réaffecter"); err != nil {
					return err
				}
				continue
			}
			if parcel.Status != "loaded" {
				continue
			}
			from := parcel.CurrentAgencyID
			if parcel, err = q.SetParcelState(ctx, store.SetParcelStateParams{ID: parcel.ID, Status: "in_transit",
				CurrentAgencyID: nil, DepartureID: parcel.DepartureID}); err != nil {
				return err
			}
			if err := s.event(ctx, q, parcel, "departed", from, &departureID, nil, ""); err != nil {
				return err
			}
		case "cancelled":
			if err := s.backToAgency(ctx, q, parcel, nil, "unassigned", "Départ annulé : à réaffecter"); err != nil {
				return err
			}
		case "arrived":
			if parcel.Status != "in_transit" {
				continue
			}
			// Agence de destination desservie par le car : déchargé là. Sinon correspondance au terminus.
			// Ligne qui finit hors agence (gare routière) : le colis reste à décharger à la main.
			at := dep.DestinationAgencyID
			if _, err := q.DepartureStopAtAgency(ctx, store.DepartureStopAtAgencyParams{DepartureID: departureID,
				AgencyID: &parcel.DestinationAgencyID}); err == nil {
				at = &parcel.DestinationAgencyID
			}
			if at != nil {
				if err := s.arrive(ctx, q, parcel, *at, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// OnStopDeparture : le car quitte un arrêt situé à une agence (journal des arrêts). Les colis
// chargés à cette agence partent ; ceux affectés mais non chargés restent sur place.
func (s *Service) OnStopDeparture(ctx context.Context, q *store.Queries, departureID, agencyID uuid.UUID) error {
	parcels, err := q.ListParcelsOnDeparture(ctx, &departureID)
	if err != nil {
		return err
	}
	for _, parcel := range parcels {
		if parcel.CurrentAgencyID == nil || *parcel.CurrentAgencyID != agencyID {
			continue
		}
		switch parcel.Status {
		case "assigned":
			if err := s.backToAgency(ctx, q, parcel, nil, "left_behind", "Non chargé au passage du bus : à réaffecter"); err != nil {
				return err
			}
		case "loaded":
			moved, err := q.SetParcelState(ctx, store.SetParcelStateParams{ID: parcel.ID, Status: "in_transit",
				CurrentAgencyID: nil, DepartureID: parcel.DepartureID})
			if err != nil {
				return err
			}
			if err := s.event(ctx, q, moved, "departed", &agencyID, &departureID, nil, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// OnStopArrival : le car arrive à un arrêt situé à une agence (journal des arrêts) ; les colis
// destinés à cette agence y sont déchargés (prêts au retrait, code envoyé au destinataire).
func (s *Service) OnStopArrival(ctx context.Context, q *store.Queries, departureID, agencyID uuid.UUID) error {
	parcels, err := q.ListParcelsOnDeparture(ctx, &departureID)
	if err != nil {
		return err
	}
	for _, parcel := range parcels {
		if parcel.Status == "in_transit" && parcel.DestinationAgencyID == agencyID {
			if err := s.arrive(ctx, q, parcel, agencyID, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// arrive : le colis est déchargé dans une agence. Agence finale : disponible au
// retrait, code envoyé au destinataire. Sinon : correspondance à organiser.
func (s *Service) arrive(ctx context.Context, q *store.Queries, parcel store.Parcel, agencyID uuid.UUID, staffID *uuid.UUID) error {
	dep := parcel.DepartureID
	status := "arrived"
	if agencyID == parcel.DestinationAgencyID {
		status = "ready"
	}
	parcel, err := q.SetParcelState(ctx, store.SetParcelStateParams{ID: parcel.ID, Status: status, CurrentAgencyID: &agencyID, DepartureID: nil})
	if err != nil {
		return err
	}
	if err := s.event(ctx, q, parcel, "arrived", &agencyID, dep, staffID, ""); err != nil {
		return err
	}
	if status == "ready" {
		return s.sendPickupCode(ctx, q, parcel, staffID)
	}
	return nil
}

// Receive : réception par scan à l'arrivée (si le départ n'a pas été déclaré arrivé).
func (s *Service) Receive(ctx context.Context, p auth.Principal, tracking string, agencyID *uuid.UUID) (store.Parcel, error) {
	var parcel store.Parcel
	err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		at, err := staffAgency(p, agencyID)
		if err != nil {
			return err
		}
		parcel, err = q.GetParcelByTrackingForUpdate(ctx, tracking)
		if database.IsNotFound(err) || (err == nil && parcel.CompanyID != p.CompanyID) {
			return apix.NotFound("Colis")
		}
		if err != nil {
			return err
		}
		if parcel.Status != "in_transit" && parcel.Status != "loaded" {
			return huma.Error409Conflict("Ce colis n'est pas en route (" + statusLabels[parcel.Status] + ")")
		}
		if parcel.DepartureID != nil {
			if dep, err := q.GetDepartureRouteAgencies(ctx, *parcel.DepartureID); err == nil && dep.DestinationAgencyID != nil && *dep.DestinationAgencyID != at {
				return huma.Error409Conflict("Ce colis voyage vers une autre agence : ne pas le décharger ici")
			}
		}
		staffID := p.ID
		if err := s.arrive(ctx, q, parcel, at, &staffID); err != nil {
			return err
		}
		parcel, err = q.GetParcelForUpdate(ctx, parcel.ID)
		return err
	})
	return parcel, err
}

func (s *Service) codeHash(parcel store.Parcel, code string) string {
	m := hmac.New(sha256.New, s.Cfg.OTPSecret)
	m.Write([]byte("parcel:" + parcel.ID.String() + ":" + code))
	return hex.EncodeToString(m.Sum(nil))
}

// sendPickupCode génère un code de retrait à 6 chiffres et le transmet au destinataire.
func (s *Service) sendPickupCode(ctx context.Context, q *store.Queries, parcel store.Parcel, staffID *uuid.UUID) error {
	code := refgen.Digits(6)
	if err := q.SetParcelPickupCode(ctx, store.SetParcelPickupCodeParams{ID: parcel.ID, CodeHash: ptrStr(s.codeHash(parcel, code))}); err != nil {
		return err
	}
	agency, err := q.GetAgency(ctx, store.GetAgencyParams{ID: parcel.DestinationAgencyID, CompanyID: parcel.CompanyID})
	if err != nil {
		return err
	}
	due := ""
	if parcel.Payer == "recipient" && parcel.PaidAt == nil {
		due = fmt.Sprintf(" Montant à régler au retrait : %s.", notify.FormatMoney(parcel.Price, parcel.Currency))
	}
	s.notify(ctx, q, parcel, "recipient", "parcel_ready", fmt.Sprintf(
		"votre colis %s de %s est disponible à %s (%s). Code de retrait : %s. Présentez-le avec une pièce d'identité.%s",
		parcel.TrackingNumber, parcel.SenderName, agency.Name, agency.Address, code, due))
	return s.event(ctx, q, parcel, "notified", &parcel.DestinationAgencyID, nil, staffID, "Code de retrait envoyé au destinataire")
}

// ResendCode renvoie un nouveau code de retrait (l'ancien n'est plus valable).
func (s *Service) ResendCode(ctx context.Context, p auth.Principal, id uuid.UUID) error {
	return s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		parcel, err := loadParcel(ctx, q, p, id)
		if err != nil {
			return err
		}
		if parcel.Status != "ready" {
			return huma.Error409Conflict("Le colis n'est pas disponible au retrait")
		}
		staffID := p.ID
		return s.sendPickupCode(ctx, q, parcel, &staffID)
	})
}

type ParcelDeliverInput struct {
	Code             string `json:"code,omitempty" pattern:"^[0-9]{6}$" doc:"Code de retrait reçu par le destinataire"`
	IdentityDocument string `json:"identity_document,omitempty" maxLength:"120" doc:"À défaut de code : type et numéro de la pièce d'identité contrôlée"`
	ReceivedBy       string `json:"received_by" minLength:"2" maxLength:"120" doc:"Nom de la personne qui retire le colis"`
	Signature        string `json:"signature" minLength:"100" maxLength:"400000" doc:"Signature (data URL PNG)"`
	CollectPayment   bool   `json:"collect_payment,omitempty" doc:"Port dû : montant encaissé en espèces"`
}

// Deliver remet le colis : code de retrait (ou pièce d'identité contrôlée,
// journalisée) + signature ; encaissement du port dû le cas échéant.
func (s *Service) Deliver(ctx context.Context, p auth.Principal, id uuid.UUID, in ParcelDeliverInput) error {
	if !strings.HasPrefix(in.Signature, "data:image/png;base64,") {
		return huma.Error422UnprocessableEntity("Signature manquante")
	}
	if in.Code == "" && len(strings.TrimSpace(in.IdentityDocument)) < 5 {
		return huma.Error422UnprocessableEntity("Saisissez le code de retrait, ou la pièce d'identité contrôlée")
	}
	return s.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
		parcel, err := loadParcel(ctx, q, p, id)
		if err != nil {
			return err
		}
		if parcel.Status != "ready" {
			return huma.Error409Conflict("Le colis n'est pas disponible au retrait (" + statusLabels[parcel.Status] + ")")
		}
		if p.Role != auth.RoleCompanyAdmin && (p.AgencyID == nil || parcel.CurrentAgencyID == nil || *p.AgencyID != *parcel.CurrentAgencyID) {
			return huma.Error403Forbidden("Le colis doit être remis par son agence de retrait")
		}
		proof, identity := "otp", (*string)(nil)
		if in.Code != "" {
			if parcel.PickupCodeHash == nil || !hmac.Equal([]byte(*parcel.PickupCodeHash), []byte(s.codeHash(parcel, in.Code))) {
				return huma.Error400BadRequest("Code de retrait incorrect")
			}
		} else {
			proof, identity = "identity", &in.IdentityDocument
		}
		if parcel.Payer == "recipient" && parcel.PaidAt == nil {
			if !in.CollectPayment {
				return huma.Error409Conflict(fmt.Sprintf("Port dû : encaissez %s avant la remise", notify.FormatMoney(parcel.Price, parcel.Currency)))
			}
			if err := s.cashPayment(ctx, tx, q, p, parcel, *parcel.CurrentAgencyID); err != nil {
				return err
			}
			if err := q.MarkParcelPaid(ctx, parcel.ID); err != nil {
				return err
			}
		}
		agencyID := parcel.CurrentAgencyID
		parcel, err = q.MarkParcelDelivered(ctx, store.MarkParcelDeliveredParams{ID: parcel.ID, DeliveredBy: &p.ID,
			DeliveredToName: &in.ReceivedBy, DeliveryProof: &proof, DeliveryIdentity: identity, Signature: &in.Signature})
		if err != nil {
			return err
		}
		note := "Remis à " + in.ReceivedBy
		if proof == "identity" {
			note += " (sans code, pièce contrôlée : " + in.IdentityDocument + ")"
		}
		staffID := p.ID
		if err := s.event(ctx, q, parcel, "delivered", agencyID, nil, &staffID, note); err != nil {
			return err
		}
		s.notify(ctx, q, parcel, "sender", "parcel_delivered",
			fmt.Sprintf("votre colis %s a été remis à %s le %s.", parcel.TrackingNumber, in.ReceivedBy, time.Now().In(localTZ).Format("02/01 à 15h04")))
		return apix.Audit(ctx, q, &p.CompanyID, "parcel.delivered", "parcel", parcel.ID.String(), nil,
			map[string]any{"proof": proof, "received_by": in.ReceivedBy})
	})
}

// Cancel annule un colis encore en agence de dépôt (non parti).
func (s *Service) Cancel(ctx context.Context, p auth.Principal, id uuid.UUID, reason string) error {
	return s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		parcel, err := loadParcel(ctx, q, p, id)
		if err != nil {
			return err
		}
		if parcel.Status != "accepted" {
			return huma.Error409Conflict("Seul un colis encore à l'agence de dépôt, non affecté, peut être annulé")
		}
		if parcel, err = q.SetParcelState(ctx, store.SetParcelStateParams{ID: parcel.ID, Status: "cancelled",
			CurrentAgencyID: parcel.CurrentAgencyID}); err != nil {
			return err
		}
		staffID := p.ID
		if err := s.event(ctx, q, parcel, "cancelled", parcel.CurrentAgencyID, nil, &staffID, reason); err != nil {
			return err
		}
		return apix.Audit(ctx, q, &p.CompanyID, "parcel.cancelled", "parcel", parcel.ID.String(), nil, map[string]any{"reason": reason})
	})
}

// ReturnToSender : colis non retiré, rendu à l'expéditeur (à l'agence de retrait ou après retour).
func (s *Service) ReturnToSender(ctx context.Context, p auth.Principal, id uuid.UUID, note string) error {
	return s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		parcel, err := loadParcel(ctx, q, p, id)
		if err != nil {
			return err
		}
		if parcel.Status != "ready" && parcel.Status != "arrived" && parcel.Status != "accepted" {
			return huma.Error409Conflict("Le colis doit se trouver en agence")
		}
		agencyID := parcel.CurrentAgencyID
		if parcel, err = q.SetParcelState(ctx, store.SetParcelStateParams{ID: parcel.ID, Status: "returned", CurrentAgencyID: nil}); err != nil {
			return err
		}
		staffID := p.ID
		if err := s.event(ctx, q, parcel, "returned", agencyID, nil, &staffID, note); err != nil {
			return err
		}
		return apix.Audit(ctx, q, &p.CompanyID, "parcel.returned", "parcel", parcel.ID.String(), nil, map[string]any{"note": note})
	})
}

func ptrStr(v string) *string { return &v }
