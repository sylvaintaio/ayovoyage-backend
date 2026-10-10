package parcel

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
	qrcode "github.com/skip2/go-qrcode"

	"busvoyage/internal/notify"
)

// TrackingFromScan accepte le numéro saisi ou le contenu du QR code de
// l'étiquette (lien de suivi public …/colis/suivi/CL…).
func TrackingFromScan(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToUpper(raw)
}

var categoryLabels = map[string]string{
	"document": "Document", "small": "Petit colis", "medium": "Colis moyen", "large": "Gros colis", "fragile": "FRAGILE",
}

// parcelLabel : étiquette 100 × 150 mm (format des imprimantes thermiques),
// une page par pièce. Le QR code renvoie vers le suivi public.
func (s *Service) parcelLabel(d ParcelDetail) ([]byte, error) {
	png, err := qrcode.Encode(s.trackingLink(d.TrackingNumber), qrcode.Medium, 512)
	if err != nil {
		return nil, err
	}
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "mm", Size: fpdf.SizeType{Wd: 100, Ht: 150}})
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	pdf.SetMargins(6, 6, 6)
	pdf.SetAutoPageBreak(false, 0)
	pdf.RegisterImageOptionsReader("qr", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(png))
	for piece := int32(1); piece <= d.Pieces; piece++ {
		pdf.AddPage()
		pdf.SetFont("Helvetica", "B", 13)
		pdf.CellFormat(0, 7, tr(d.CompanyName), "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 8)
		pdf.CellFormat(0, 4, tr("Colis — "+d.CreatedAt.In(localTZ).Format("02/01/2006 15h04")), "", 1, "L", false, 0, "")
		pdf.Ln(2)

		pdf.SetFillColor(18, 22, 20)
		pdf.SetTextColor(255, 255, 255)
		pdf.SetFont("Helvetica", "B", 20)
		pdf.CellFormat(0, 13, tr(d.DestinationCity), "", 1, "C", true, 0, "")
		pdf.SetFont("Helvetica", "", 9)
		pdf.CellFormat(0, 6, tr("Retrait : "+d.DestinationAgencyName), "", 1, "C", true, 0, "")
		pdf.SetTextColor(18, 22, 20)
		pdf.Ln(3)

		pdf.ImageOptions("qr", 27, pdf.GetY(), 46, 46, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
		pdf.SetY(pdf.GetY() + 47)
		pdf.SetFont("Courier", "B", 16)
		pdf.CellFormat(0, 7, d.TrackingNumber, "", 1, "C", false, 0, "")
		pdf.SetFont("Helvetica", "B", 10)
		pdf.CellFormat(0, 6, tr(fmt.Sprintf("Pièce %d / %d", piece, d.Pieces)), "", 1, "C", false, 0, "")
		pdf.Ln(2)

		line := func(label, value string) {
			pdf.SetFont("Helvetica", "", 8)
			pdf.CellFormat(24, 5, tr(label), "", 0, "L", false, 0, "")
			pdf.SetFont("Helvetica", "B", 9)
			pdf.CellFormat(0, 5, tr(value), "", 1, "L", false, 0, "")
		}
		line("Destinataire", d.RecipientName+"  "+d.RecipientPhone)
		line("Expéditeur", d.SenderName)
		line("Départ", d.OriginAgencyName)
		line("Contenu", fmt.Sprintf("%s — %s, %.1f kg", categoryLabels[d.Category], d.Description, d.WeightKg))
		payment := "Port payé"
		if !d.Paid {
			payment = "PORT DÛ : " + notify.FormatMoney(d.Price, d.Currency) + " à encaisser au retrait"
		}
		line("Paiement", strings.ReplaceAll(strings.ReplaceAll(payment, " ", " "), " ", " "))
		if d.Category == "fragile" {
			pdf.Ln(2)
			pdf.SetFont("Helvetica", "B", 14)
			pdf.SetTextColor(180, 35, 24)
			pdf.CellFormat(0, 8, tr("FRAGILE — MANIPULER AVEC SOIN"), "1", 1, "C", false, 0, "")
			pdf.SetTextColor(18, 22, 20)
		}
	}
	var buf bytes.Buffer
	err = pdf.Output(&buf)
	return buf.Bytes(), err
}

// baggageLabel : étiquette bagage 100 × 70 mm à attacher à la poignée.
func baggageLabel(company, tag, passenger, seat, route, destination, date, booking string, weight float64) ([]byte, error) {
	png, err := qrcode.Encode(tag, qrcode.Medium, 384)
	if err != nil {
		return nil, err
	}
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "mm", Size: fpdf.SizeType{Wd: 100, Ht: 70}})
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	pdf.SetMargins(5, 5, 5)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddPage()
	pdf.RegisterImageOptionsReader("qr", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(png))
	pdf.ImageOptions("qr", 5, 12, 34, 34, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 6, tr(company+" — BAGAGE"), "", 1, "L", false, 0, "")
	pdf.SetXY(42, 12)
	pdf.SetFont("Helvetica", "B", 18)
	pdf.CellFormat(0, 9, tr(destination), "", 2, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	for _, l := range []string{passenger + " — siège " + seat, route, date, fmt.Sprintf("%.1f kg — réservation %s", weight, booking)} {
		pdf.SetX(42)
		pdf.CellFormat(0, 5, tr(l), "", 2, "L", false, 0, "")
	}
	pdf.SetXY(5, 50)
	pdf.SetFont("Courier", "B", 16)
	pdf.CellFormat(34, 7, tag, "", 0, "C", false, 0, "")
	var buf bytes.Buffer
	err = pdf.Output(&buf)
	return buf.Bytes(), err
}
