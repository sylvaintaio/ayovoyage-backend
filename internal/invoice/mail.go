package invoice

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"busvoyage/internal/jobs"
	"busvoyage/internal/notify"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// Mailer envoie les factures et avoirs par e-mail, PDF A4 en pièce jointe.
// Chaque envoi est inscrit dans « Mes messages » du client.
type Mailer struct {
	DB *database.DB
	N  *notify.Notifier
}

// attachment produit le PDF A4 d'une facture.
func attachment(ctx context.Context, q *store.Queries, inv store.Invoice) (notify.Attachment, error) {
	out, err := render(ctx, q, inv, "a4")
	if err != nil {
		return notify.Attachment{}, err
	}
	return notify.Attachment{Name: inv.Number + ".pdf", ContentType: "application/pdf", Data: out.Body}, nil
}

// compose construit l'e-mail d'une facture, PDF joint.
func (m *Mailer) compose(ctx context.Context, q *store.Queries, inv store.Invoice, to string) (notify.Email, error) {
	att, err := attachment(ctx, q, inv)
	if err != nil {
		return notify.Email{}, err
	}
	brand := m.N.Brand
	brand.Name = inv.SellerName
	link := ""
	if inv.CustomerID != nil && m.N.WebURL != "" {
		link = strings.TrimRight(m.N.WebURL, "/") + "/compte/factures"
	}
	msg := notify.InvoiceEmail(brand, to, notify.InvoiceView{
		CustomerName: inv.CustomerName, Number: inv.Number, CreditNote: inv.Kind == "credit_note",
		Reference: inv.Reference, Amount: inv.TotalAmount, Currency: inv.Currency, IssuedAt: inv.IssuedAt, Link: link,
	})
	msg.Attachments = []notify.Attachment{att}
	return msg, nil
}

// remember inscrit l'envoi dans les messages du client.
func (m *Mailer) remember(ctx context.Context, q *store.Queries, inv store.Invoice, msg notify.Email, sendErr error) {
	template := "invoice"
	if inv.Kind == "credit_note" {
		template = "credit_note"
	}
	companyID := inv.CompanyID
	m.N.Record(ctx, q, &companyID, inv.CustomerID, template, msg.To, msg.Subject, msg.Text, sendErr)
}

// send : compose, envoie et inscrit dans les messages (sans mettre à jour le suivi d'envoi).
func (m *Mailer) send(ctx context.Context, q *store.Queries, inv store.Invoice, to string) error {
	msg, err := m.compose(ctx, q, inv, to)
	if err != nil {
		return err
	}
	sendErr := m.N.Email.SendEmail(ctx, msg)
	m.remember(ctx, q, inv, msg, sendErr)
	return sendErr
}

// BookingInvoices : factures d'une réservation encore jamais envoyées, à joindre
// à l'e-mail de confirmation du billet (branché sur notify.Notifier).
func BookingInvoices(ctx context.Context, q *store.Queries, bookingID uuid.UUID) ([]notify.Attachment, []uuid.UUID) {
	rows, err := q.ListBookingInvoices(ctx, &bookingID)
	if err != nil {
		slog.Error("factures à joindre", "booking", bookingID, "err", err)
		return nil, nil
	}
	var atts []notify.Attachment
	var ids []uuid.UUID
	for _, inv := range rows {
		if inv.Kind != "invoice" || inv.EmailStatus != "none" || inv.EmailedAt != nil {
			continue
		}
		att, err := attachment(ctx, q, inv)
		if err != nil {
			slog.Error("facture PDF", "invoice", inv.Number, "err", err)
			continue
		}
		atts = append(atts, att)
		ids = append(ids, inv.ID)
	}
	return atts, ids
}

// EmailWorker envoie les factures en attente (bagages, colis, avoirs).
// Échec : nouvel essai 5, 10, 15 puis 20 minutes plus tard.
type EmailWorker struct {
	river.WorkerDefaults[jobs.SendInvoiceEmailsArgs]
	M *Mailer
}

func (w *EmailWorker) Work(ctx context.Context, _ *river.Job[jobs.SendInvoiceEmailsArgs]) error {
	return w.M.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		pending, err := q.ClaimInvoiceEmails(ctx)
		if err != nil {
			return err
		}
		var errs []error
		for _, inv := range pending {
			sendErr := w.M.send(ctx, q, inv, inv.EmailTo)
			if sendErr != nil {
				slog.Warn("facture non envoyée", "invoice", inv.Number, "essai", inv.EmailAttempts+1, "err", sendErr)
			}
			result, errMsg := notify.DeliveryStatus(w.M.N.Email, sendErr)
			if err := q.SetInvoiceEmailResult(ctx, store.SetInvoiceEmailResultParams{ID: inv.ID, EmailTo: inv.EmailTo, EmailError: errMsg, Result: result}); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}
