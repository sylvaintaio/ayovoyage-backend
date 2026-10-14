// Package ticket émet les billets (QR signé), génère le PDF et expose
// les billets du client.
package ticket

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	qrcode "github.com/skip2/go-qrcode"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/refgen"
	"busvoyage/internal/store"
	"busvoyage/internal/ticketsign"
)

const (
	tagTickets = "Billets"
	tagAdmin   = "Admin — Réservations"
)

type Service struct {
	DB     *database.DB
	Signer *ticketsign.Signer
}

// IssueForBooking crée un billet signé pour chaque siège vendu de la réservation.
func (s *Service) IssueForBooking(ctx context.Context, q *store.Queries, b store.Booking, seats []store.BookingSeat) error {
	for _, seat := range seats {
		id := uuid.New()
		qr, err := s.Signer.Sign(ticketsign.Payload{
			TicketID: id, DepartureID: b.DepartureID, CompanyID: b.CompanyID, Seat: seat.SeatLabel,
		})
		if err != nil {
			return err
		}
		if _, err := q.InsertTicket(ctx, store.InsertTicketParams{
			ID: id, CompanyID: b.CompanyID, BookingID: b.ID, BookingSeatID: seat.ID,
			DepartureID: b.DepartureID, TicketNumber: refgen.Ticket(), QrPayload: qr,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- DTO

type Ticket struct {
	ID            uuid.UUID  `json:"id"`
	TicketNumber  string     `json:"ticket_number"`
	BookingID     uuid.UUID  `json:"booking_id"`
	DepartureID   uuid.UUID  `json:"departure_id"`
	SeatLabel     string     `json:"seat_label"`
	PassengerName string     `json:"passenger_name"`
	Status        string     `json:"status" enum:"valid,used,cancelled"`
	QRPayload     string     `json:"qr_payload" doc:"Contenu à encoder dans le QR code (signé Ed25519)"`
	BoardedAt     *time.Time `json:"boarded_at,omitempty"`
	IssuedAt      time.Time  `json:"issued_at"`
	DepartureAt   *time.Time `json:"departure_at,omitempty"`
	RouteName     string     `json:"route_name,omitempty"`
	CompanyName   string     `json:"company_name,omitempty"`
}

type TicketDetail struct {
	Ticket
	CategoryName          string    `json:"category_name"`
	BookingReference      string    `json:"booking_reference"`
	ArrivalEta            time.Time `json:"arrival_eta"`
	OriginCity            string    `json:"origin_city"`
	OriginAgencyName      string    `json:"origin_agency_name"`
	OriginAgencyAddress   string    `json:"origin_agency_address"`
	DestinationCity       string    `json:"destination_city"`
	DestinationAgencyName string    `json:"destination_agency_name"`
	IncludedBaggageKg     int32     `json:"included_baggage_kg"`
	PlateNumber           string    `json:"bus_plate_number"`
	Price                 int64     `json:"price"`
	Currency              string    `json:"currency"`
}

func toDetail(t store.GetTicketDetailRow) TicketDetail {
	dep := t.DepartureAt
	return TicketDetail{
		Ticket: Ticket{ID: t.ID, TicketNumber: t.TicketNumber, BookingID: t.BookingID, DepartureID: t.DepartureID,
			SeatLabel: t.SeatLabel, PassengerName: t.PassengerName, Status: t.Status, QRPayload: t.QrPayload,
			BoardedAt: t.BoardedAt, IssuedAt: t.IssuedAt, DepartureAt: &dep, RouteName: t.RouteName, CompanyName: t.CompanyName},
		CategoryName: t.CategoryName, BookingReference: t.BookingReference, ArrivalEta: t.ArrivalEta,
		OriginCity: t.OriginCity, OriginAgencyName: t.OriginAgencyName, OriginAgencyAddress: t.OriginAgencyAddress,
		DestinationCity: t.DestinationCity, DestinationAgencyName: t.DestinationAgencyName,
		IncludedBaggageKg: t.IncludedBaggageKg, PlateNumber: t.PlateNumber, Price: t.TotalPrice, Currency: t.Currency,
	}
}

type pdfOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	CacheControl       string `header:"Cache-Control"`
	Body               []byte
}

// ---------------------------------------------------------------- Routes

func (s *Service) Register(api huma.API) {
	huma.Register(api, apix.Op("list-my-tickets", http.MethodGet, "/v1/tickets", tagTickets, "Mes billets", apix.Customer),
		func(ctx context.Context, in *struct {
			apix.Page
			Upcoming bool `query:"upcoming" doc:"Uniquement les voyages à venir"`
		}) (*struct{ Body []Ticket }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Ticket }{Body: []Ticket{}}
			err := s.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCustomerTickets(ctx, store.ListCustomerTicketsParams{
					CustomerID: &p.ID, UpcomingOnly: in.Upcoming, Lim: in.Limit, Off: in.Offset,
				})
				for _, r := range rows {
					dep := r.DepartureAt
					out.Body = append(out.Body, Ticket{ID: r.ID, TicketNumber: r.TicketNumber, BookingID: r.BookingID,
						DepartureID: r.DepartureID, SeatLabel: r.SeatLabel, PassengerName: r.PassengerName, Status: r.Status,
						QRPayload: r.QrPayload, BoardedAt: r.BoardedAt, IssuedAt: r.IssuedAt, DepartureAt: &dep,
						RouteName: r.RouteName, CompanyName: r.CompanyName})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-my-ticket", http.MethodGet, "/v1/tickets/{id}", tagTickets, "Détail d'un billet", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body TicketDetail }, error) {
			t, err := s.customerTicket(ctx, in.ID)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body TicketDetail }{Body: toDetail(t)}, nil
		})

	huma.Register(api, apix.Op("get-my-ticket-pdf", http.MethodGet, "/v1/tickets/{id}/pdf", tagTickets, "Télécharger le billet en PDF", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*pdfOutput, error) {
			t, err := s.customerTicket(ctx, in.ID)
			if err != nil {
				return nil, apix.Err(err)
			}
			return renderPDFOutput(ctx, t)
		})

	huma.Register(api, apix.Op("get-ticket-pdf-staff", http.MethodGet, "/v1/admin/tickets/{id}/pdf", tagAdmin, "Imprimer un billet (guichet)", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*pdfOutput, error) {
			p := auth.MustFromContext(ctx)
			var t store.GetTicketDetailRow
			err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				t, err = q.GetTicketDetail(ctx, in.ID)
				return err
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			return renderPDFOutput(ctx, t)
		})
}

// customerTicket charge un billet en vérifiant qu'il appartient au client connecté.
func (s *Service) customerTicket(ctx context.Context, id uuid.UUID) (store.GetTicketDetailRow, error) {
	p := auth.MustFromContext(ctx)
	var t store.GetTicketDetailRow
	err := s.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
		var err error
		t, err = q.GetTicketDetail(ctx, id)
		if err == nil && (t.CustomerID == nil || *t.CustomerID != p.ID) {
			return apix.NotFound("Billet") // ne révèle pas l'existence du billet d'un autre client
		}
		return err
	})
	return t, err
}

func renderPDFOutput(ctx context.Context, t store.GetTicketDetailRow) (*pdfOutput, error) {
	pdf, err := RenderPDF(ctx, t)
	if err != nil {
		return nil, apix.Err(err)
	}
	return &pdfOutput{
		ContentType:        "application/pdf",
		ContentDisposition: fmt.Sprintf(`attachment; filename="billet-%s.pdf"`, t.TicketNumber),
		CacheControl:       "private, no-store",
		Body:               pdf,
	}, nil
}

// ---------------------------------------------------------------- PDF

var localTZ = time.FixedZone("WAT", 3600)

// money : « 12 500 F CFA ».
func money(amount int64, currency string) string {
	digits := strconv.FormatInt(amount, 10)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	if currency == "" || currency == "XOF" {
		currency = "F CFA"
	}
	return b.String() + " " + currency
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// RenderPDF produit le billet (format A6) : compagnie (logo, nom, coordonnées), trajet du voyageur
// (montée et descente, heures), QR code, voyageur et place, mentions légales de la compagnie.
func RenderPDF(ctx context.Context, t store.GetTicketDetailRow) ([]byte, error) {
	png, err := qrcode.Encode(t.QrPayload, qrcode.Medium, 512)
	if err != nil {
		return nil, err
	}
	const margin, width = 7.0, 105.0 - 14.0
	pdf := fpdf.New("P", "mm", "A6", "")
	cp1252 := pdf.UnicodeTranslatorFromDescriptor("") // UTF-8 → cp1252 (accents)
	// Flèches des noms de ligne (« Cotonou → Parakou ») : absentes de la police, remplacées.
	tr := func(s string) string { return cp1252(strings.NewReplacer("→", "-", "←", "-").Replace(s)) }
	pdf.SetMargins(margin, margin, margin)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddPage()
	pdf.SetTextColor(18, 22, 20)

	// --- Compagnie : logo (si disponible), nom, téléphone
	y := margin
	if logo := companyLogo(ctx, t.CompanyLogoUrl); logo != nil {
		pdf.RegisterImageOptionsReader("logo", fpdf.ImageOptions{ImageType: logo.kind}, bytes.NewReader(logo.data))
		if info := pdf.GetImageInfo("logo"); info != nil && info.Height() > 0 {
			h := 12.0
			w := min(info.Width()*h/info.Height(), width/2)
			h = w * info.Height() / info.Width()
			pdf.ImageOptions("logo", (105-w)/2, y, w, h, false, fpdf.ImageOptions{ImageType: logo.kind}, 0, "")
			y += h + 1.5
		}
	}
	pdf.SetXY(margin, y)
	pdf.SetFont("Helvetica", "B", 13)
	pdf.CellFormat(0, 6, tr(t.CompanyName), "", 1, "C", false, 0, "")
	if phone := str(t.CompanyPhone); phone != "" {
		pdf.SetFont("Helvetica", "", 8)
		pdf.CellFormat(0, 4, tr("Tél. "+phone), "", 1, "C", false, 0, "")
	}
	pdf.Ln(1.5)

	// --- Bandeau d'état
	status := map[string]string{"valid": "BILLET VALIDE", "used": "BILLET UTILISÉ", "cancelled": "BILLET ANNULÉ"}[t.Status]
	switch t.Status {
	case "valid":
		pdf.SetFillColor(11, 77, 58)
	case "cancelled":
		pdf.SetFillColor(180, 35, 24)
	default:
		pdf.SetFillColor(91, 102, 96)
	}
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.CellFormat(0, 6, tr(status+"  ·  "+t.TicketNumber), "", 1, "C", true, 0, "")
	pdf.SetTextColor(18, 22, 20)
	pdf.Ln(2)

	// --- Trajet : montée → descente
	col := (width - 10) / 2
	top := pdf.GetY()
	block := func(x float64, label, when, date, stop string) {
		pdf.SetXY(x, top)
		pdf.SetFont("Helvetica", "", 7)
		pdf.SetTextColor(91, 102, 96)
		pdf.CellFormat(col, 3.5, tr(label), "", 2, "L", false, 0, "")
		pdf.SetTextColor(18, 22, 20)
		pdf.SetFont("Helvetica", "B", 15)
		pdf.CellFormat(col, 7, tr(when), "", 2, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 7.5)
		pdf.CellFormat(col, 3.5, tr(date), "", 2, "L", false, 0, "")
		pdf.SetFont("Helvetica", "B", 8)
		pdf.MultiCell(col, 3.6, tr(stop), "", "L", false)
	}
	dep, arr := t.DepartureAt.In(localTZ), t.ArrivalEta.In(localTZ)
	block(margin, "MONTÉE", dep.Format("15h04"), dep.Format("02/01/2006"), t.OriginAgencyName)
	leftEnd := pdf.GetY()
	arrDate := ""
	if arr.YearDay() != dep.YearDay() {
		arrDate = arr.Format("02/01/2006")
	}
	block(margin+col+10, "DESCENTE (heure prévue)", arr.Format("15h04"), arrDate, t.DestinationAgencyName)
	rightEnd := pdf.GetY()
	// Flèche entre les deux blocs.
	ax, ay := margin+col+0.5, top+7
	pdf.SetDrawColor(245, 183, 0)
	pdf.SetLineWidth(0.8)
	pdf.Line(ax, ay, ax+5.5, ay)
	pdf.SetFillColor(245, 183, 0)
	pdf.Polygon([]fpdf.PointType{{X: ax + 5.5, Y: ay - 1.6}, {X: ax + 7.5, Y: ay}, {X: ax + 5.5, Y: ay + 1.6}}, "F")
	pdf.SetY(max(leftEnd, rightEnd) + 1)
	if addr := strings.TrimSpace(t.OriginAgencyAddress); addr != "" {
		pdf.SetFont("Helvetica", "", 7)
		pdf.SetTextColor(91, 102, 96)
		pdf.MultiCell(0, 3.4, tr("Lieu de montée : "+addr), "", "L", false)
		pdf.SetTextColor(18, 22, 20)
	}
	pdf.Ln(1)

	// --- QR code
	pdf.RegisterImageOptionsReader("qr", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(png))
	qr := 36.0
	pdf.ImageOptions("qr", (105-qr)/2, pdf.GetY(), qr, qr, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
	pdf.SetY(pdf.GetY() + qr + 1)

	// --- Voyageur et place
	row := func(label, value string) {
		pdf.SetFont("Helvetica", "", 7.5)
		pdf.SetTextColor(91, 102, 96)
		pdf.CellFormat(26, 4.4, tr(label), "", 0, "L", false, 0, "")
		pdf.SetTextColor(18, 22, 20)
		pdf.SetFont("Helvetica", "B", 8.5)
		pdf.CellFormat(0, 4.4, tr(value), "", 1, "L", false, 0, "")
	}
	row("Voyageur", t.PassengerName)
	row("Siège", t.SeatLabel+" · "+t.CategoryName)
	row("Réservation", t.BookingReference)
	row("Car", t.PlateNumber+" · "+t.RouteName)
	row("Bagage inclus", fmt.Sprintf("%d kg", t.IncludedBaggageKg))
	row("Prix", money(t.TotalPrice, t.Currency))

	// --- Consignes et mentions de la compagnie
	pdf.Ln(1)
	pdf.SetFont("Helvetica", "I", 6.5)
	pdf.MultiCell(0, 3.1, tr("Présentez ce QR code à l'embarquement, 30 minutes avant le départ. "+
		"Billet nominatif vérifié électroniquement : une copie déjà scannée est refusée."), "", "C", false)
	legal := []string{}
	for _, v := range []string{str(t.CompanyLegalName), str(t.CompanyAddress)} {
		if v != "" {
			legal = append(legal, v)
		}
	}
	if v := str(t.CompanyTaxID); v != "" {
		legal = append(legal, "IFU "+v)
	}
	if v := str(t.CompanyEmail); v != "" {
		legal = append(legal, v)
	}
	if len(legal) > 0 {
		pdf.SetFont("Helvetica", "", 6.5)
		pdf.SetTextColor(91, 102, 96)
		pdf.MultiCell(0, 3.1, tr(strings.Join(legal, " · ")), "", "C", false)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
