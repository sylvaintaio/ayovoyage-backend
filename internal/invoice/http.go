package invoice

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/notify"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

const tag = "Factures"

type Handler struct {
	DB     *database.DB
	Mailer *Mailer
}

type InvoiceLine struct {
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Detail    string `json:"detail,omitempty"`
	Quantity  int32  `json:"quantity"`
	UnitPrice int64  `json:"unit_price"`
	Amount    int64  `json:"amount"`
}

type Invoice struct {
	ID              uuid.UUID     `json:"id"`
	Kind            string        `json:"kind" enum:"invoice,credit_note"`
	Number          string        `json:"number"`
	IssuedAt        time.Time     `json:"issued_at"`
	Reference       string        `json:"reference"`
	BookingID       *uuid.UUID    `json:"booking_id,omitempty"`
	ParcelID        *uuid.UUID    `json:"parcel_id,omitempty"`
	CustomerName    string        `json:"customer_name"`
	CustomerPhone   string        `json:"customer_phone,omitempty"`
	CustomerCompany string        `json:"customer_company,omitempty"`
	CustomerTaxID   string        `json:"customer_tax_id,omitempty"`
	AgencyName      string        `json:"agency_name,omitempty"`
	OperatorName    string        `json:"operator_name"`
	PaymentMethod   string        `json:"payment_method"`
	Currency        string        `json:"currency"`
	TotalAmount     int64         `json:"total_amount"`
	EmecefStatus    string        `json:"emecef_status" enum:"none,pending,certified,failed" doc:"Facture normalisée DGI (none = facture interne)"`
	CustomerEmail   string        `json:"customer_email,omitempty"`
	EmailStatus     string        `json:"email_status" enum:"none,pending,sent,simulated,failed" doc:"Envoi par e-mail : none = pas encore envoyée"`
	EmailTo         string        `json:"email_to,omitempty"`
	EmailedAt       *time.Time    `json:"emailed_at,omitempty"`
	Lines           []InvoiceLine `json:"lines,omitempty"`
}

func toInvoice(i store.Invoice) Invoice {
	return Invoice{ID: i.ID, Kind: i.Kind, Number: i.Number, IssuedAt: i.IssuedAt, Reference: i.Reference, BookingID: i.BookingID,
		ParcelID: i.ParcelID, CustomerName: i.CustomerName, CustomerPhone: i.CustomerPhone, CustomerCompany: i.CustomerCompany,
		CustomerTaxID: i.CustomerTaxID, AgencyName: i.AgencyName, OperatorName: i.OperatorName, PaymentMethod: i.PaymentMethod,
		Currency: i.Currency, TotalAmount: i.TotalAmount, EmecefStatus: i.EmecefStatus,
		CustomerEmail: i.CustomerEmail, EmailStatus: i.EmailStatus, EmailTo: i.EmailTo, EmailedAt: i.EmailedAt}
}

func list(rows []store.Invoice) []Invoice {
	out := make([]Invoice, 0, len(rows))
	for _, r := range rows {
		out = append(out, toInvoice(r))
	}
	return out
}

type pdfOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	CacheControl       string `header:"Cache-Control"`
	Body               []byte
}

// render charge la facture, ses lignes et la facture d'origine (avoir), puis produit le PDF.
func render(ctx context.Context, q *store.Queries, inv store.Invoice, format string) (*pdfOutput, error) {
	lines, err := q.ListInvoiceLines(ctx, inv.ID)
	if err != nil {
		return nil, err
	}
	var original *store.Invoice
	if inv.OriginalInvoiceID != nil {
		if o, err := q.GetInvoice(ctx, *inv.OriginalInvoiceID); err == nil {
			original = &o
		}
	}
	var pdf []byte
	if format == "ticket" {
		pdf, err = RenderTicket(inv, lines, original)
	} else {
		pdf, err = RenderA4(inv, lines, original)
	}
	if err != nil {
		return nil, err
	}
	return &pdfOutput{ContentType: "application/pdf", CacheControl: "private, no-store",
		ContentDisposition: fmt.Sprintf(`inline; filename="%s.pdf"`, inv.Number), Body: pdf}, nil
}

var (
	readers = []string{auth.RoleCompanyAdmin, auth.RoleFinance, auth.RoleAgencyManager, auth.RoleCashier, auth.RoleParcelAgent}
	finance = []string{auth.RoleCompanyAdmin, auth.RoleFinance}
)

func period(from, to string) (time.Time, time.Time, error) {
	now := time.Now().In(localTZ)
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, localTZ)
	end := start.AddDate(0, 1, 0)
	var err error
	if from != "" {
		if start, err = time.ParseInLocation(time.DateOnly, from, localTZ); err != nil {
			return start, end, huma.Error422UnprocessableEntity("Date de début invalide")
		}
	}
	if to != "" {
		var t time.Time
		if t, err = time.ParseInLocation(time.DateOnly, to, localTZ); err != nil {
			return start, end, huma.Error422UnprocessableEntity("Date de fin invalide")
		}
		end = t.AddDate(0, 0, 1)
	}
	return start, end, nil
}

func (h *Handler) Register(api huma.API) {
	type listInput struct {
		apix.Page
		From   string `query:"from" format:"date" doc:"Par défaut : premier jour du mois"`
		To     string `query:"to" format:"date" doc:"Inclus. Par défaut : fin du mois"`
		Kind   string `query:"kind" enum:"invoice,credit_note,"`
		Search string `query:"q" maxLength:"60" doc:"Numéro, référence, client ou téléphone"`
	}

	huma.Register(api, apix.Op("list-invoices", http.MethodGet, "/v1/admin/invoices", tag, "Factures et avoirs", apix.Staff, readers...),
		func(ctx context.Context, in *listInput) (*struct {
			Body struct {
				Invoices         []Invoice `json:"invoices"`
				Invoiced         int64     `json:"invoiced" doc:"Total facturé sur la période"`
				Credited         int64     `json:"credited" doc:"Total des avoirs"`
				InvoicesCount    int32     `json:"invoices_count"`
				CreditNotesCount int32     `json:"credit_notes_count"`
			}
		}, error) {
			p := auth.MustFromContext(ctx)
			from, to, err := period(in.From, in.To)
			if err != nil {
				return nil, err
			}
			out := &struct {
				Body struct {
					Invoices         []Invoice `json:"invoices"`
					Invoiced         int64     `json:"invoiced" doc:"Total facturé sur la période"`
					Credited         int64     `json:"credited" doc:"Total des avoirs"`
					InvoicesCount    int32     `json:"invoices_count"`
					CreditNotesCount int32     `json:"credit_notes_count"`
				}
			}{}
			err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListInvoices(ctx, store.ListInvoicesParams{CompanyID: p.CompanyID, Kind: apix.NilIfEmpty(in.Kind),
					FromTs: from, ToTs: to, Search: apix.NilIfEmpty(in.Search), Lim: in.Limit, Off: in.Offset})
				if err != nil {
					return err
				}
				out.Body.Invoices = list(rows)
				t, err := q.InvoiceTotals(ctx, store.InvoiceTotalsParams{CompanyID: p.CompanyID, FromTs: from, ToTs: to})
				out.Body.Invoiced, out.Body.Credited, out.Body.InvoicesCount, out.Body.CreditNotesCount = t.Invoiced, t.Credited, t.InvoicesCount, t.CreditNotesCount
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-invoice", http.MethodGet, "/v1/admin/invoices/{id}", tag, "Détail d'une facture", apix.Staff, readers...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body Invoice }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body Invoice }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				inv, err := q.GetInvoice(ctx, in.ID)
				if err != nil || inv.CompanyID != p.CompanyID {
					return apix.NotFound("Facture")
				}
				out.Body = toInvoice(inv)
				lines, err := q.ListInvoiceLines(ctx, inv.ID)
				for _, l := range lines {
					out.Body.Lines = append(out.Body.Lines, InvoiceLine{Kind: l.Kind, Label: l.Label, Detail: l.Detail, Quantity: l.Quantity,
						UnitPrice: l.UnitPrice, Amount: l.Amount})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-invoice-pdf", http.MethodGet, "/v1/admin/invoices/{id}/pdf", tag, "Facture en PDF (A4 ou ticket 80 mm)", apix.Staff, readers...),
		func(ctx context.Context, in *struct {
			ID     uuid.UUID `path:"id"`
			Format string    `query:"format" enum:"a4,ticket," doc:"a4 (par défaut) ou ticket (imprimante 80 mm)"`
		}) (*pdfOutput, error) {
			p := auth.MustFromContext(ctx)
			var out *pdfOutput
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				inv, err := q.GetInvoice(ctx, in.ID)
				if err != nil || inv.CompanyID != p.CompanyID {
					return apix.NotFound("Facture")
				}
				out, err = render(ctx, q, inv, in.Format)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("export-invoices", http.MethodGet, "/v1/admin/invoices/export", tag,
		"Export des factures (CSV)", apix.Staff, finance...),
		"Pour le comptable : une ligne par facture ou avoir, séparateur point-virgule (Excel), montants en francs CFA."),
		func(ctx context.Context, in *struct {
			From string `query:"from" format:"date"`
			To   string `query:"to" format:"date"`
		}) (*struct {
			ContentType        string `header:"Content-Type"`
			ContentDisposition string `header:"Content-Disposition"`
			Body               []byte
		}, error) {
			p := auth.MustFromContext(ctx)
			from, to, err := period(in.From, in.To)
			if err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			buf.WriteString("\ufeff") // BOM : accents corrects dans Excel
			w := csv.NewWriter(&buf)
			w.Comma = ';'
			_ = w.Write([]string{"Numéro", "Type", "Date", "Référence", "Client", "Entreprise", "IFU client", "Téléphone", "Agence",
				"Opérateur", "Paiement", "Montant", "Devise", "Normalisée DGI"})
			err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListInvoices(ctx, store.ListInvoicesParams{CompanyID: p.CompanyID, FromTs: from, ToTs: to, Lim: 100000})
				for _, r := range rows {
					kind, amount := "Facture", r.TotalAmount
					if r.Kind == "credit_note" {
						kind, amount = "Avoir", -r.TotalAmount
					}
					_ = w.Write([]string{r.Number, kind, r.IssuedAt.In(localTZ).Format("02/01/2006 15:04"), r.Reference, r.CustomerName,
						r.CustomerCompany, r.CustomerTaxID, r.CustomerPhone, r.AgencyName, r.OperatorName, r.PaymentMethod,
						strconv.FormatInt(amount, 10), r.Currency, map[bool]string{true: "oui", false: "non"}[r.EmecefStatus == "certified"]})
				}
				return err
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			w.Flush()
			return &struct {
				ContentType        string `header:"Content-Type"`
				ContentDisposition string `header:"Content-Disposition"`
				Body               []byte
			}{ContentType: "text/csv; charset=utf-8",
				ContentDisposition: fmt.Sprintf(`attachment; filename="factures_%s_%s.csv"`, from.Format("2006-01-02"), to.AddDate(0, 0, -1).Format("2006-01-02")),
				Body:               buf.Bytes()}, nil
		})

	huma.Register(api, apix.Op("list-booking-invoices", http.MethodGet, "/v1/admin/bookings/{id}/invoices", tag, "Factures d'une réservation", apix.Staff, readers...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body []Invoice }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Invoice }{Body: []Invoice{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListBookingInvoices(ctx, &in.ID)
				for _, r := range rows {
					if r.CompanyID == p.CompanyID {
						out.Body = append(out.Body, toInvoice(r))
					}
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-parcel-invoices", http.MethodGet, "/v1/admin/parcels/{id}/invoices", tag, "Factures d'un colis", apix.Staff, readers...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body []Invoice }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Invoice }{Body: []Invoice{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListParcelInvoices(ctx, &in.ID)
				for _, r := range rows {
					if r.CompanyID == p.CompanyID {
						out.Body = append(out.Body, toInvoice(r))
					}
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("email-invoice", http.MethodPost, "/v1/admin/invoices/{id}/email", tag,
		"Envoyer une facture par e-mail", apix.Staff, readers...),
		"PDF A4 en pièce jointe. Sans adresse : celle du client figurant sur la facture. L'envoi apparaît dans les messages du client."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				To string `json:"to,omitempty" format:"email" maxLength:"254" doc:"Autre destinataire (comptable de l'entreprise…)"`
			}
		}) (*struct{ Body Invoice }, error) {
			p := auth.MustFromContext(ctx)
			var inv store.Invoice
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				if inv, err = q.GetInvoice(ctx, in.ID); err != nil {
					return apix.NotFound("Facture")
				}
				return nil
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			to := strings.TrimSpace(in.Body.To)
			if to == "" {
				to = inv.CustomerEmail
			}
			if to == "" {
				return nil, huma.Error422UnprocessableEntity("Le client n'a pas d'adresse e-mail : indiquez un destinataire.")
			}
			return h.sendNow(ctx, p.Tenant(), inv, to, func(q *store.Queries) error {
				return apix.Audit(ctx, q, &inv.CompanyID, "invoice.emailed", "invoice", inv.ID.String(), nil, map[string]string{"to": to})
			})
		})

	// --- Espace client
	huma.Register(api, apix.Op("list-my-invoices", http.MethodGet, "/v1/me/invoices", tag, "Mes factures", apix.Customer),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Invoice }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Invoice }{Body: []Invoice{}}
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCustomerInvoices(ctx, &p.ID)
				out.Body = list(rows)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-my-booking-invoices", http.MethodGet, "/v1/bookings/{id}/invoices", tag, "Factures de ma réservation", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body []Invoice }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Invoice }{Body: []Invoice{}}
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				b, err := q.GetBooking(ctx, in.ID)
				if err != nil || b.CustomerID == nil || *b.CustomerID != p.ID {
					return apix.NotFound("Réservation")
				}
				rows, err := q.ListBookingInvoices(ctx, &b.ID)
				out.Body = list(rows)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("email-my-invoice", http.MethodPost, "/v1/me/invoices/{id}/email", tag,
		"Recevoir ma facture par e-mail", apix.Customer),
		"Envoyée à l'adresse du compte, PDF en pièce jointe. Une demande toutes les 2 minutes au plus par facture."),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body Invoice }, error) {
			p := auth.MustFromContext(ctx)
			var inv store.Invoice
			to := ""
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				if inv, err = q.GetInvoice(ctx, in.ID); err != nil || !ownedBy(ctx, q, inv, p.ID) {
					return apix.NotFound("Facture")
				}
				c, err := q.GetCustomer(ctx, p.ID)
				if err != nil {
					return err
				}
				if c.Email != nil {
					to = strings.TrimSpace(*c.Email)
				}
				return nil
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			if to == "" {
				return nil, huma.Error422UnprocessableEntity("Ajoutez une adresse e-mail à votre profil pour recevoir vos factures.")
			}
			if inv.EmailedAt != nil && time.Since(*inv.EmailedAt) < 2*time.Minute {
				return nil, huma.Error429TooManyRequests("Facture déjà envoyée il y a moins de 2 minutes : vérifiez vos courriers indésirables.")
			}
			return h.sendNow(ctx, database.System(), inv, to, nil)
		})

	huma.Register(api, apix.Op("get-my-invoice-pdf", http.MethodGet, "/v1/me/invoices/{id}/pdf", tag, "Télécharger ma facture (PDF)", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*pdfOutput, error) {
			p := auth.MustFromContext(ctx)
			var out *pdfOutput
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				inv, err := q.GetInvoice(ctx, in.ID)
				if err != nil || !ownedBy(ctx, q, inv, p.ID) {
					return apix.NotFound("Facture")
				}
				out, err = render(ctx, q, inv, "a4")
				return err
			})
			return out, apix.Err(err)
		})
}

// sendNow envoie la facture hors transaction (SMTP lent), puis enregistre le
// résultat et l'inscrit dans les messages du client. Pas de nouvel essai automatique.
func (h *Handler) sendNow(ctx context.Context, tenant database.Tenant, inv store.Invoice, to string, after func(q *store.Queries) error) (*struct{ Body Invoice }, error) {
	m := h.Mailer
	if m == nil || m.N == nil {
		return nil, huma.Error503ServiceUnavailable("Envoi d'e-mails indisponible")
	}
	var msg notify.Email
	if err := h.DB.Tx(ctx, tenant, func(_ pgx.Tx, q *store.Queries) error {
		var err error
		msg, err = m.compose(ctx, q, inv, to)
		return err
	}); err != nil {
		return nil, apix.Err(err)
	}
	sendErr := m.N.Email.SendEmail(ctx, msg)
	result, errMsg := notify.DeliveryStatus(m.N.Email, sendErr)
	out := &struct{ Body Invoice }{}
	err := h.DB.Tx(ctx, tenant, func(_ pgx.Tx, q *store.Queries) error {
		m.remember(ctx, q, inv, msg, sendErr)
		if err := q.RecordInvoiceEmail(ctx, store.RecordInvoiceEmailParams{ID: inv.ID, EmailTo: to, EmailError: errMsg, Result: result}); err != nil {
			return err
		}
		if after != nil {
			if err := after(q); err != nil {
				return err
			}
		}
		updated, err := q.GetInvoice(ctx, inv.ID)
		out.Body = toInvoice(updated)
		return err
	})
	if err != nil {
		return nil, apix.Err(err)
	}
	if sendErr != nil {
		slog.Warn("facture non envoyée", "invoice", inv.Number, "err", sendErr)
		return nil, huma.Error502BadGateway("L'e-mail n'a pas pu être envoyé. Réessayez dans quelques minutes.")
	}
	return out, nil
}

// ownedBy : facture du client (directement, ou via une de ses réservations).
func ownedBy(ctx context.Context, q *store.Queries, inv store.Invoice, customerID uuid.UUID) bool {
	if inv.CustomerID != nil && *inv.CustomerID == customerID {
		return true
	}
	if inv.BookingID != nil {
		if b, err := q.GetBooking(ctx, *inv.BookingID); err == nil && b.CustomerID != nil && *b.CustomerID == customerID {
			return true
		}
	}
	return false
}
