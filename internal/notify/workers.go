package notify

import (
	"busvoyage/internal/push"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"busvoyage/internal/jobs"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/review"
	"busvoyage/internal/segment"
	"busvoyage/internal/store"
)

type Notifier struct {
	Brand Brand
	DB    *database.DB
	SMS   SMSSender
	Email EmailSender
	// Push : notifications sur les téléphones (nil : désactivé).
	Push   push.Sender
	WebURL string
	// BookingInvoices : factures PDF à joindre à l'e-mail de confirmation
	// (fourni par le package invoice ; nil = aucune pièce jointe).
	BookingInvoices func(ctx context.Context, q *store.Queries, bookingID uuid.UUID) ([]Attachment, []uuid.UUID)
}

// Record : version exportée de record, pour les envois faits hors de ce package (factures).
func (n *Notifier) Record(ctx context.Context, q *store.Queries, companyID, customerID *uuid.UUID, template, to, subject, body string, sendErr error) {
	n.record(ctx, q, companyID, customerID, "email", template, to, subject, body, sendErr)
}

// Les horaires sont affichés à l'heure locale du Bénin (UTC+1, sans heure d'été).
var localTZ = time.FixedZone("WAT", 3600)

func formatDate(t time.Time) string {
	return t.In(localTZ).Format("02/01/2006 à 15h04")
}

// record enregistre un envoi dans l'historique du client (sujet et texte
// compris) : c'est ce que le client retrouve dans « Mes messages ».
func (n *Notifier) record(ctx context.Context, q *store.Queries, companyID, customerID *uuid.UUID, channel, template, to, subject, body string, sendErr error) {
	if errors.Is(sendErr, ErrNoRecipient) {
		return
	}
	sender := any(n.SMS)
	if channel == "email" {
		sender = n.Email
	}
	status, errMsg := DeliveryStatus(sender, sendErr)
	if err := q.InsertNotification(ctx, store.InsertNotificationParams{
		CompanyID: companyID, CustomerID: customerID, Channel: channel, Template: template,
		Recipient: to, Status: status, Error: errMsg, Subject: subject, Body: body,
	}); err != nil {
		slog.Error("historique des messages", "template", template, "err", err)
	}
}

func (n *Notifier) log(ctx context.Context, q *store.Queries, b store.Booking, channel, template, to, subject, body string, sendErr error) {
	companyID := b.CompanyID
	n.record(ctx, q, &companyID, b.CustomerID, channel, template, to, subject, body, sendErr)
}

// ---------------------------------------------------------------- Bienvenue

type WelcomeWorker struct {
	river.WorkerDefaults[jobs.WelcomeArgs]
	N *Notifier
}

func (w *WelcomeWorker) Work(ctx context.Context, job *river.Job[jobs.WelcomeArgs]) error {
	n := w.N
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		c, err := q.GetCustomer(ctx, job.Args.CustomerID)
		if err != nil {
			return err
		}
		// Boîte de réception de l'application (et notification si le téléphone est déjà inscrit).
		name := strings.TrimSpace(c.FullName)
		hello := "Bienvenue"
		if name != "" {
			hello += " " + strings.Fields(name)[0]
		}
		n.inboxQuiet(ctx, q, Note{CustomerID: &c.ID, Kind: "welcome", Title: hello + " sur " + n.Brand.Name,
			Body:     "Cherchez un trajet, choisissez vos places et payez par Mobile Money. Vos billets restent dans l'application, même sans réseau.",
			DedupKey: "welcome"})
		if c.Email == nil || *c.Email == "" {
			return nil // pas d'e-mail : rien à envoyer tant qu'aucun fournisseur SMS n'est branché
		}
		if sent, err := q.WelcomeAlreadySent(ctx, &c.ID); err != nil || sent {
			return err
		}
		msg := WelcomeEmail(n.Brand, *c.Email, c.FullName)
		sendErr := n.Email.SendEmail(ctx, msg)
		n.record(ctx, q, nil, &c.ID, "email", "welcome", *c.Email, msg.Subject, msg.Text, sendErr)
		return sendErr // en cas d'échec, River réessaie plus tard
	})
}

// ---------------------------------------------------------------- Réservation confirmée

type BookingConfirmedWorker struct {
	river.WorkerDefaults[jobs.BookingConfirmedArgs]
	N *Notifier
}

func (w *BookingConfirmedWorker) Work(ctx context.Context, job *river.Job[jobs.BookingConfirmedArgs]) error {
	n := w.N
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		b, err := q.GetBooking(ctx, job.Args.BookingID)
		if err != nil {
			return err
		}
		d, err := q.GetDepartureDetail(ctx, b.DepartureID)
		if err != nil {
			return err
		}
		// Trajet du voyageur (arrêt de montée → arrêt de descente), pas les extrémités de la ligne.
		if err := segment.ForBooking(ctx, q, &d, b.ID); err != nil {
			return err
		}
		tickets, err := q.ListBookingTickets(ctx, b.ID)
		if err != nil {
			return err
		}
		seats := make([]string, 0, len(tickets))
		for _, t := range tickets {
			seats = append(seats, t.SeatLabel)
		}
		link := fmt.Sprintf("%s/compte/reservations/%s", n.WebURL, b.ID)
		template, oldRef := "booking_confirmed", ""
		sms := fmt.Sprintf("%s : réservation %s confirmée. %s → %s le %s, siège(s) %s. Billet : %s",
			d.CompanyName, b.Reference, d.OriginCity, d.DestinationCity, formatDate(d.DepartureAt),
			strings.Join(seats, ", "), link)
		if b.ChangedFromBookingID != nil {
			if old, err := q.GetBooking(ctx, *b.ChangedFromBookingID); err == nil {
				oldRef = old.Reference
			}
			template = "booking_changed"
			sms = fmt.Sprintf("%s : voyage modifié. Nouvelle réservation %s : %s → %s le %s, siège(s) %s. Les anciens billets ne sont plus valables. Billets : %s",
				d.CompanyName, b.Reference, d.OriginCity, d.DestinationCity, formatDate(d.DepartureAt),
				strings.Join(seats, ", "), link)
		}
		n.log(ctx, q, b, "sms", template, b.ContactPhone, "", sms, n.SMS.SendSMS(ctx, b.ContactPhone, sms))
		title := "Réservation confirmée"
		if template == "booking_changed" {
			title = "Voyage modifié"
		}
		n.inboxQuiet(ctx, q, Note{CustomerID: b.CustomerID, CompanyID: &b.CompanyID, Kind: template, Title: title,
			Body: fmt.Sprintf("%s → %s le %s, siège(s) %s. Réf. %s", d.OriginCity, d.DestinationCity, formatDate(d.DepartureAt),
				strings.Join(seats, ", "), b.Reference),
			Link: "/reservations/" + b.ID.String(), Data: map[string]string{"booking_id": b.ID.String()},
			DedupKey: template + ":" + b.ID.String()})

		if b.ContactEmail != nil && *b.ContactEmail != "" {
			brand := n.Brand
			brand.Name = d.CompanyName
			view := BookingView{CustomerName: b.ContactName, Reference: b.Reference, Origin: d.OriginCity,
				Destination: d.DestinationCity, DepartureDate: FrenchDate(d.DepartureAt.In(localTZ)),
				DepartureTime: d.DepartureAt.In(localTZ).Format("15:04"), Agency: d.OriginAgencyName,
				Amount: b.TotalAmount, Currency: b.Currency, Link: link,
				Changed: b.ChangedFromBookingID != nil, OldReference: oldRef}
			for _, t := range tickets {
				view.Passengers = append(view.Passengers, Passenger{Seat: t.SeatLabel, Name: t.PassengerName, Ticket: t.TicketNumber})
			}
			var attachments []Attachment
			var invoiceIDs []uuid.UUID
			if n.BookingInvoices != nil {
				attachments, invoiceIDs = n.BookingInvoices(ctx, q, b.ID)
			}
			view.InvoiceAttached = len(attachments) > 0
			msg := BookingEmail(brand, *b.ContactEmail, view)
			msg.Attachments = attachments
			sendErr := n.Email.SendEmail(ctx, msg)
			n.log(ctx, q, b, "email", template, *b.ContactEmail, msg.Subject, msg.Text, sendErr)
			result, errMsg := DeliveryStatus(n.Email, sendErr)
			for _, id := range invoiceIDs {
				if err := q.RecordInvoiceEmail(ctx, store.RecordInvoiceEmailParams{ID: id, EmailTo: *b.ContactEmail, EmailError: errMsg, Result: result}); err != nil {
					slog.Error("suivi d'envoi de facture", "invoice", id, "err", err)
				}
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------- Réservation annulée

type BookingCancelledWorker struct {
	river.WorkerDefaults[jobs.BookingCancelledArgs]
	N *Notifier
}

func (w *BookingCancelledWorker) Work(ctx context.Context, job *river.Job[jobs.BookingCancelledArgs]) error {
	n := w.N
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		b, err := q.GetBooking(ctx, job.Args.BookingID)
		if err != nil {
			return err
		}
		msg := fmt.Sprintf("Votre réservation %s a été annulée. %s", b.Reference, b.CancelReason)
		n.log(ctx, q, b, "sms", "booking_cancelled", b.ContactPhone, "", msg, n.SMS.SendSMS(ctx, b.ContactPhone, msg))
		n.inboxQuiet(ctx, q, Note{CustomerID: b.CustomerID, CompanyID: &b.CompanyID, Kind: "booking_cancelled",
			Title: "Réservation annulée", Body: msg, Link: "/reservations/" + b.ID.String(),
			Data: map[string]string{"booking_id": b.ID.String()}, DedupKey: "booking_cancelled:" + b.ID.String()})
		return nil
	})
}

// ---------------------------------------------------------------- Changement de statut d'un départ

type DepartureStatusWorker struct {
	river.WorkerDefaults[jobs.DepartureStatusArgs]
	N *Notifier
}

var departureMessages = map[string]string{
	"boarding":    "L'embarquement de votre bus %s → %s (%s) est ouvert.",
	"departed":    "Votre bus %s → %s (%s) est parti.",
	"arrived":     "Votre bus %s → %s (%s) est arrivé à destination.",
	"cancelled":   "Le départ %s → %s du %s est annulé. Vous serez contacté pour un remboursement ou un report.",
	"rescheduled": "Changement d'horaire : votre bus %s → %s passera désormais à %s le %s.",
}

var departureTitles = map[string]string{
	"boarding":    "Embarquement ouvert",
	"departed":    "Votre car est parti",
	"arrived":     "Arrivée",
	"cancelled":   "Départ annulé",
	"rescheduled": "Changement d'horaire",
}

var staffStatus = map[string]string{"cancelled": "départ annulé", "rescheduled": "nouvel horaire"}

func (w *DepartureStatusWorker) Work(ctx context.Context, job *river.Job[jobs.DepartureStatusArgs]) error {
	n := w.N
	tpl, ok := departureMessages[job.Args.Status]
	if !ok {
		return nil
	}
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		d, err := q.GetDepartureDetail(ctx, job.Args.DepartureID)
		if err != nil {
			return err
		}
		// « Embarquement ouvert » ne concerne que les voyageurs qui montent au premier arrêt.
		contacts, err := q.ListDepartureContacts(ctx, store.ListDepartureContactsParams{DepartureID: d.ID,
			FirstStopOnly: job.Args.Status == "boarding"})
		if err != nil {
			return err
		}
		// Équipage : départ annulé ou déplacé.
		if job.Args.Status == "cancelled" || job.Args.Status == "rescheduled" {
			crew, err := q.ListDepartureStaffIDs(ctx, d.ID)
			if err != nil {
				return err
			}
			for _, c := range crew {
				staffID, companyID := c.StaffID, d.CompanyID
				n.inboxQuiet(ctx, q, Note{StaffID: &staffID, CompanyID: &companyID, Kind: "crew_departure_" + job.Args.Status,
					Title: departureTitles[job.Args.Status],
					Body:  fmt.Sprintf("%s du %s : %s.", d.RouteName, formatDate(d.DepartureAt), staffStatus[job.Args.Status]),
					Link:  "/departs/" + d.ID.String(), Data: map[string]string{"departure_id": d.ID.String()},
					DedupKey: fmt.Sprintf("crew_%s:%s:%d", job.Args.Status, d.ID, d.DepartureAt.Unix())})
			}
		}
		for _, c := range contacts {
			msg := fmt.Sprintf(tpl, d.OriginCity, d.DestinationCity, formatDate(d.DepartureAt))
			if job.Args.Status == "rescheduled" { // nouvelle heure de passage à l'arrêt de montée du voyageur
				msg = fmt.Sprintf(tpl, d.OriginCity, d.DestinationCity, c.BoardingStop, formatDate(c.BoardingAt))
			}
			companyID := d.CompanyID
			n.inboxQuiet(ctx, q, Note{CustomerID: c.CustomerID, CompanyID: &companyID, Kind: "departure_" + job.Args.Status,
				Title: departureTitles[job.Args.Status], Body: msg, Link: "/reservations",
				Data:     map[string]string{"departure_id": d.ID.String()},
				DedupKey: fmt.Sprintf("departure_%s:%s:%d", job.Args.Status, d.ID, c.BoardingAt.Unix())})
			err := n.SMS.SendSMS(ctx, c.ContactPhone, msg)
			if errors.Is(err, ErrNoRecipient) {
				continue
			}
			status, errMsg := DeliveryStatus(n.SMS, err)
			_ = q.InsertNotification(ctx, store.InsertNotificationParams{
				CompanyID: &companyID, CustomerID: c.CustomerID, Channel: "sms",
				Template: "departure_" + job.Args.Status, Recipient: c.ContactPhone, Status: status, Error: errMsg, Body: msg,
			})
		}
		return nil
	})
}

// ---------------------------------------------------------------- Rappel la veille du voyage

type RemindersWorker struct {
	river.WorkerDefaults[jobs.SendRemindersArgs]
	N *Notifier
}

// Deux rappels par réservation :
//   - la veille : départ dans 12 à 26 h, envoyé entre 7 h et 21 h ;
//   - le jour J : départ dans 3 à 7 h (vers H-6), envoyé entre 6 h et 22 h.
//
// Pas de SMS la nuit (heure du Bénin) : pour un départ très matinal, seul le
// rappel de la veille est envoyé.
func reminderHours(now time.Time) bool {
	h := now.In(localTZ).Hour()
	return h >= 7 && h < 21
}

func dayReminderHours(now time.Time) bool {
	h := now.In(localTZ).Hour()
	return h >= 6 && h < 22
}

func (w *RemindersWorker) Work(ctx context.Context, _ *river.Job[jobs.SendRemindersArgs]) error {
	n := w.N
	now := time.Now()
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		if reminderHours(now) {
			ids, err := q.ListBookingsToRemind(ctx)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := n.remind(ctx, q, id, false); err != nil {
					slog.Error("rappel de la veille", "booking", id, "err", err)
				}
			}
		}
		if dayReminderHours(now) {
			ids, err := q.ListBookingsToRemindToday(ctx)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := n.remind(ctx, q, id, true); err != nil {
					slog.Error("rappel du jour", "booking", id, "err", err)
				}
			}
		}
		return nil
	})
}

func (n *Notifier) remind(ctx context.Context, q *store.Queries, id uuid.UUID, sameDay bool) error {
	// Marqué avant l'envoi : en cas d'échec, pas de doublon au passage suivant.
	template, mark := "trip_reminder", q.MarkReminderSent
	if sameDay {
		template, mark = "trip_reminder_day", q.MarkDayReminderSent
	}
	if err := mark(ctx, id); err != nil {
		return err
	}
	b, err := q.GetBooking(ctx, id)
	if err != nil {
		return err
	}
	d, err := q.GetDepartureDetail(ctx, b.DepartureID)
	if err != nil {
		return err
	}
	// Trajet du voyageur (arrêt de montée → arrêt de descente), pas les extrémités de la ligne.
	if err := segment.ForBooking(ctx, q, &d, b.ID); err != nil {
		return err
	}
	tickets, err := q.ListBookingTickets(ctx, b.ID)
	if err != nil {
		return err
	}
	var seats []string
	for _, t := range tickets {
		if t.Status == "valid" {
			seats = append(seats, t.SeatLabel)
		}
	}
	dep := d.DepartureAt.In(localTZ)
	when := "demain"
	if dep.YearDay() == time.Now().In(localTZ).YearDay() {
		when = "aujourd’hui"
	}
	link := fmt.Sprintf("%s/compte/reservations/%s", n.WebURL, b.ID)
	sms := fmt.Sprintf("%s : rappel, départ %s %s à %s, agence %s. Venez 30 min avant avec vos billets (sièges %s). Réf. %s. %s",
		d.CompanyName, strings.ReplaceAll(when, "’", "'"), d.OriginCity, dep.Format("15h04"), d.OriginAgencyName,
		strings.Join(seats, ", "), b.Reference, link)
	if sameDay {
		hours := int(time.Until(d.DepartureAt).Round(time.Hour).Hours())
		sms = fmt.Sprintf("%s : votre bus %s → %s part à %s (dans %d h), agence %s. Arrivez 30 min avant avec vos billets (sièges %s). Réf. %s. %s",
			d.CompanyName, d.OriginCity, d.DestinationCity, dep.Format("15h04"), hours, d.OriginAgencyName,
			strings.Join(seats, ", "), b.Reference, link)
	}
	n.log(ctx, q, b, "sms", template, b.ContactPhone, "", sms, n.SMS.SendSMS(ctx, b.ContactPhone, sms))
	n.inboxQuiet(ctx, q, Note{CustomerID: b.CustomerID, CompanyID: &b.CompanyID, Kind: template,
		Title: "Rappel de voyage",
		Body: fmt.Sprintf("Départ %s %s à %s (%s). Venez 30 min avant. Sièges %s, réf. %s.", strings.ReplaceAll(when, "’", "'"),
			d.OriginCity, dep.Format("15h04"), d.OriginAgencyName, strings.Join(seats, ", "), b.Reference),
		Link: "/reservations/" + b.ID.String(), Data: map[string]string{"booking_id": b.ID.String()},
		DedupKey: template + ":" + b.ID.String()})
	if b.ContactEmail != nil && *b.ContactEmail != "" {
		brand := n.Brand
		brand.Name = d.CompanyName
		msg := ReminderEmail(brand, *b.ContactEmail, ReminderView{CustomerName: b.ContactName, Reference: b.Reference,
			Origin: d.OriginCity, Destination: d.DestinationCity, When: when, DepartureDate: FrenchDate(dep),
			DepartureTime: dep.Format("15:04"), Agency: d.OriginAgencyName, AgencyAddress: d.OriginAgencyAddress,
			Seats: seats, BaggageKg: d.IncludedBaggageKg, Link: link, SameDay: sameDay})
		n.log(ctx, q, b, "email", template, *b.ContactEmail, msg.Subject, msg.Text, n.Email.SendEmail(ctx, msg))
	}
	return nil
}

// ---------------------------------------------------------------- Purge des données expirées

type PurgeWorker struct {
	river.WorkerDefaults[jobs.PurgeDataArgs]
	DB *database.DB
}

func (w *PurgeWorker) Work(ctx context.Context, _ *river.Job[jobs.PurgeDataArgs]) error {
	return w.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		otps, err := q.PurgeExpiredOTPs(ctx)
		if err != nil {
			return err
		}
		tokens, err := q.PurgeOldRefreshTokens(ctx)
		if err != nil {
			return err
		}
		inbox, err := q.PurgeOldInbox(ctx)
		if err != nil {
			return err
		}
		slog.Info("purge des données expirées", "codes", otps, "sessions", tokens, "notifications", inbox)
		return nil
	})
}

// ---------------------------------------------------------------- Payer en agence

// PayLaterWorker : réservation à payer en agence (confirmation de blocage, expiration).
type PayLaterWorker struct {
	river.WorkerDefaults[jobs.PayLaterNoticeArgs]
	N *Notifier
}

func (w *PayLaterWorker) Work(ctx context.Context, job *river.Job[jobs.PayLaterNoticeArgs]) error {
	return w.N.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		b, err := q.GetBooking(ctx, job.Args.BookingID)
		if err != nil {
			return err
		}
		return w.N.payLater(ctx, q, b, job.Args.Event)
	})
}

// PayLaterRemindersWorker : rappel avant l'échéance (moins de 3 h, ou du quart du délai).
type PayLaterRemindersWorker struct {
	river.WorkerDefaults[jobs.SendPayLaterRemindersArgs]
	N *Notifier
}

func (w *PayLaterRemindersWorker) Work(ctx context.Context, _ *river.Job[jobs.SendPayLaterRemindersArgs]) error {
	return w.N.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		ids, err := q.ListPayLaterReminders(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			b, err := q.MarkPayLaterReminded(ctx, id)
			if err != nil {
				continue // payée ou déjà rappelée entre-temps
			}
			if err := w.N.payLater(ctx, q, b, "reminder"); err != nil {
				slog.Error("rappel paiement en agence", "booking", b.Reference, "err", err)
			}
		}
		return nil
	})
}

func (n *Notifier) payLater(ctx context.Context, q *store.Queries, b store.Booking, event string) error {
	d, err := q.GetDepartureDetail(ctx, b.DepartureID)
	if err != nil {
		return err
	}
	// Trajet du voyageur (arrêt de montée → arrêt de descente), pas les extrémités de la ligne.
	if err := segment.ForBooking(ctx, q, &d, b.ID); err != nil {
		return err
	}
	view := PayLaterView{
		Event: event, CustomerName: b.ContactName, Reference: b.Reference, Origin: d.OriginCity, Destination: d.DestinationCity,
		DepartureDate: FrenchDate(d.DepartureAt.In(localTZ)), DepartureTime: d.DepartureAt.In(localTZ).Format("15h04"),
		Amount: b.TotalAmount, Currency: b.Currency, DateShort: d.DepartureAt.In(localTZ).Format("02/01"),
		Link: fmt.Sprintf("%s/compte/reservations/%s", n.WebURL, b.ID),
	}
	if b.PayBy != nil {
		view.PayBy = FrenchDate(b.PayBy.In(localTZ)) + " à " + b.PayBy.In(localTZ).Format("15h04")
		view.PayByShort = b.PayBy.In(localTZ).Format("02/01 15h04")
	}
	if agencies, err := q.ListPayLaterAgencies(ctx, b.CompanyID); err == nil {
		for _, a := range agencies {
			label := a.Name
			if !strings.Contains(strings.ToLower(a.Name), strings.ToLower(a.City)) {
				label = a.City + " — " + a.Name
			}
			if a.Address != "" {
				label += " (" + a.Address + ")"
			}
			view.Agencies = append(view.Agencies, label)
		}
	}
	template := "pay_later_" + event
	sms := PayLaterSMS(d.CompanyName, view)
	n.log(ctx, q, b, "sms", template, b.ContactPhone, "", sms, n.SMS.SendSMS(ctx, b.ContactPhone, sms))
	if b.ContactEmail != nil && *b.ContactEmail != "" {
		brand := n.Brand
		brand.Name = d.CompanyName
		msg := PayLaterEmail(brand, *b.ContactEmail, view)
		n.log(ctx, q, b, "email", template, *b.ContactEmail, msg.Subject, msg.Text, n.Email.SendEmail(ctx, msg))
	}
	return nil
}

// ---------------------------------------------------------------- Avis

// ReviewInvitationsWorker : invite à donner son avis environ 2 h après l'arrivée
// (lien personnel valable 14 jours, sans connexion).
type ReviewInvitationsWorker struct {
	river.WorkerDefaults[jobs.SendReviewInvitationsArgs]
	N *Notifier
}

func (w *ReviewInvitationsWorker) Work(ctx context.Context, _ *river.Job[jobs.SendReviewInvitationsArgs]) error {
	n := w.N
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		rows, err := q.ListReviewCandidates(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			token, hash := review.NewToken()
			if _, err := q.InsertReviewInvitation(ctx, store.InsertReviewInvitationParams{
				CompanyID: r.CompanyID, BookingID: r.ID, TokenHash: hash, ExpiresAt: time.Now().Add(review.InvitationValidity),
			}); err != nil {
				continue // déjà invitée entre-temps
			}
			link := fmt.Sprintf("%s/avis/%s", n.WebURL, token)
			trip := r.OriginCity + " → " + r.DestinationCity
			companyID := r.CompanyID
			if r.ContactEmail != nil && *r.ContactEmail != "" {
				brand := n.Brand
				brand.Name = r.CompanyName
				msg := ReviewInviteEmail(brand, *r.ContactEmail, r.ContactName, trip, FrenchDate(r.DepartureAt.In(localTZ)), link)
				n.record(ctx, q, &companyID, r.CustomerID, "email", "review_invitation", *r.ContactEmail, msg.Subject, msg.Text,
					n.Email.SendEmail(ctx, msg))
			} else {
				sms := ReviewInviteSMS(r.CompanyName, r.OriginCity+"-"+r.DestinationCity, link)
				n.record(ctx, q, &companyID, r.CustomerID, "sms", "review_invitation", r.ContactPhone, "", sms,
					n.SMS.SendSMS(ctx, r.ContactPhone, sms))
			}
		}
		return nil
	})
}
