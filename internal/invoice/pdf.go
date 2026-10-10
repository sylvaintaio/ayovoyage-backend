package invoice

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-pdf/fpdf"
	qrcode "github.com/skip2/go-qrcode"

	"busvoyage/internal/store"
)

// money : 25000 → « 25 000 F CFA » (espaces simples : police PDF en cp1252).
func money(v int64, currency string) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatInt(v, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if currency == "XOF" || currency == "" {
		out += " F CFA"
	} else {
		out += " " + currency
	}
	if neg {
		out = "- " + out
	}
	return out
}

// pdfText remplace les caractères hors cp1252 par un équivalent lisible.
var pdfText = strings.NewReplacer("→", "-", "–", "-", "’", "'", " ", " ", " ", " ", "…", "...")

type doc struct {
	pdf *fpdf.Fpdf
	tr  func(string) string
}

func newDoc(size fpdf.SizeType, margin float64) doc {
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "mm", Size: size})
	pdf.SetMargins(margin, margin, margin)
	pdf.SetAutoPageBreak(true, margin+6)
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	return doc{pdf: pdf, tr: func(s string) string { return tr(pdfText.Replace(s)) }}
}

func title(inv store.Invoice) string {
	if inv.Kind == "credit_note" {
		return "FACTURE D'AVOIR"
	}
	return "FACTURE"
}

// Couleurs de la charte « gare routière » (mêmes valeurs que le site et les e-mails).
const (
	inkR, inkG, inkB          = 18, 22, 20    // #121614
	mutedR, mutedG, mutedB    = 75, 84, 79    // #4b544f
	greenR, greenG, greenB    = 11, 77, 58    // #0b4d3a
	lineR, lineG, lineB       = 225, 227, 223 // #e1e3df
	accentR, accentG, accentB = 245, 183, 0   // #f5b700
)

// RenderA4 : facture au format A4.
func RenderA4(inv store.Invoice, lines []store.InvoiceLine, original *store.Invoice) ([]byte, error) {
	d := newDoc(fpdf.SizeType{Wd: 210, Ht: 297}, 16)
	p, tr := d.pdf, d.tr
	p.SetFooterFunc(func() {
		p.SetY(-14)
		p.SetFont("Helvetica", "", 7)
		p.SetTextColor(mutedR, mutedG, mutedB)
		p.CellFormat(0, 4, tr(fmt.Sprintf("%s · %s · page %d", inv.SellerName, inv.Number, p.PageNo())), "", 0, "C", false, 0, "")
	})
	p.AddPage()

	// Bandeau : vert, filet jaune signal
	p.SetFillColor(greenR, greenG, greenB)
	p.Rect(0, 0, 210, 4, "F")
	p.SetFillColor(accentR, accentG, accentB)
	p.Rect(0, 4, 210, 1.5, "F")

	// Vendeur (gauche)
	p.SetY(14)
	p.SetTextColor(inkR, inkG, inkB)
	p.SetFont("Helvetica", "B", 16)
	p.CellFormat(110, 8, tr(inv.SellerName), "", 2, "L", false, 0, "")
	p.SetFont("Helvetica", "", 8.5)
	p.SetTextColor(mutedR, mutedG, mutedB)
	for _, l := range []string{inv.SellerLegalName, inv.SellerAddress,
		joinNonEmpty(" · ", prefixed("IFU ", inv.SellerTaxID), prefixed("RCCM ", inv.SellerTradeRegister)),
		joinNonEmpty(" · ", inv.SellerPhone, inv.SellerEmail)} {
		if strings.TrimSpace(l) != "" {
			p.CellFormat(110, 4.4, tr(l), "", 2, "L", false, 0, "")
		}
	}

	// Titre (droite)
	p.SetXY(120, 14)
	p.SetTextColor(greenR, greenG, greenB)
	p.SetFont("Helvetica", "B", 18)
	p.CellFormat(74, 8, tr(title(inv)), "", 2, "R", false, 0, "")
	p.SetTextColor(inkR, inkG, inkB)
	p.SetFont("Helvetica", "B", 10)
	p.SetX(120)
	p.CellFormat(74, 5.5, tr("N° "+inv.Number), "", 2, "R", false, 0, "")
	p.SetFont("Helvetica", "", 8.5)
	p.SetTextColor(mutedR, mutedG, mutedB)
	p.SetX(120)
	p.CellFormat(74, 4.4, tr("Date : "+inv.IssuedAt.In(localTZ).Format("02/01/2006 à 15h04")), "", 2, "R", false, 0, "")
	if inv.Reference != "" {
		p.SetX(120)
		p.CellFormat(74, 4.4, tr("Référence : "+inv.Reference), "", 2, "R", false, 0, "")
	}
	if original != nil {
		p.SetX(120)
		p.CellFormat(74, 4.4, tr("Avoir sur la facture "+original.Number), "", 2, "R", false, 0, "")
	}

	// Client + contexte
	y := 50.0
	box := func(x, w float64, heading string, rows []string) {
		p.SetXY(x, y)
		p.SetFont("Helvetica", "B", 7.5)
		p.SetTextColor(mutedR, mutedG, mutedB)
		p.CellFormat(w, 5, tr(strings.ToUpper(heading)), "", 2, "L", false, 0, "")
		p.SetTextColor(inkR, inkG, inkB)
		for i, r := range rows {
			if strings.TrimSpace(r) == "" {
				continue
			}
			p.SetX(x)
			if i == 0 {
				p.SetFont("Helvetica", "B", 10)
			} else {
				p.SetFont("Helvetica", "", 8.5)
			}
			p.CellFormat(w, 5, tr(r), "", 2, "L", false, 0, "")
		}
	}
	client := []string{inv.CustomerName}
	if inv.CustomerCompany != "" {
		client = []string{inv.CustomerCompany, prefixed("IFU ", inv.CustomerTaxID), "Contact : " + inv.CustomerName}
	}
	client = append(client, inv.CustomerPhone, inv.CustomerEmail)
	box(16, 95, "Facturé à", client)
	box(120, 74, "Vente", []string{inv.PaymentMethod, prefixed("Agence : ", inv.AgencyName), prefixed("Par : ", inv.OperatorName)})

	// Lignes
	p.SetY(86)
	p.SetFillColor(242, 248, 245)
	p.SetFont("Helvetica", "B", 8)
	p.SetTextColor(mutedR, mutedG, mutedB)
	p.CellFormat(104, 8, tr("  DÉSIGNATION"), "", 0, "L", true, 0, "")
	p.CellFormat(14, 8, tr("QTÉ"), "", 0, "C", true, 0, "")
	p.CellFormat(30, 8, tr("PRIX UNITAIRE"), "", 0, "R", true, 0, "")
	p.CellFormat(30, 8, tr("MONTANT  "), "", 1, "R", true, 0, "")
	p.SetDrawColor(lineR, lineG, lineB)
	for _, l := range lines {
		top := p.GetY()
		p.SetX(16)
		p.SetTextColor(inkR, inkG, inkB)
		p.SetFont("Helvetica", "B", 9)
		p.MultiCell(104, 5, tr("  "+l.Label), "", "L", false)
		if l.Detail != "" {
			p.SetX(16)
			p.SetFont("Helvetica", "", 7.5)
			p.SetTextColor(mutedR, mutedG, mutedB)
			p.MultiCell(104, 4, tr("  "+l.Detail), "", "L", false)
		}
		bottom := p.GetY() + 2
		p.SetTextColor(inkR, inkG, inkB)
		p.SetFont("Helvetica", "", 9)
		p.SetXY(120, top)
		p.CellFormat(14, 5, strconv.Itoa(int(l.Quantity)), "", 0, "C", false, 0, "")
		p.CellFormat(30, 5, tr(money(l.UnitPrice, inv.Currency)), "", 0, "R", false, 0, "")
		p.SetFont("Helvetica", "B", 9)
		p.CellFormat(30, 5, tr(money(l.Amount, inv.Currency)+"  "), "", 0, "R", false, 0, "")
		p.Line(16, bottom, 194, bottom)
		p.SetY(bottom + 1)
	}

	// Total
	p.Ln(3)
	label := "Total payé"
	if inv.Kind == "credit_note" {
		label = "Montant remboursé"
	}
	p.SetX(120)
	p.SetFillColor(greenR, greenG, greenB)
	p.SetTextColor(255, 255, 255)
	p.SetFont("Helvetica", "B", 11)
	p.CellFormat(44, 10, tr("  "+label), "", 0, "L", true, 0, "")
	p.CellFormat(30, 10, tr(money(inv.TotalAmount, inv.Currency)+"  "), "", 1, "R", true, 0, "")

	// Facture normalisée (e-MECeF), si certifiée
	afterTotal := p.GetY() + 8
	if inv.EmecefStatus == "certified" && inv.EmecefQr != nil {
		if png, err := qrcode.Encode(*inv.EmecefQr, qrcode.Medium, 384); err == nil {
			p.RegisterImageOptionsReader("dgi", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(png))
			p.ImageOptions("dgi", 16, afterTotal, 30, 30, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
			p.SetXY(50, afterTotal+2)
			p.SetTextColor(inkR, inkG, inkB)
			p.SetFont("Helvetica", "B", 9)
			p.CellFormat(0, 5, tr("Facture normalisée — DGI Bénin"), "", 2, "L", false, 0, "")
			p.SetFont("Helvetica", "", 8)
			for _, l := range []string{prefixed("Code MECeF/DGI : ", deref(inv.EmecefCode)), prefixed("NIM : ", deref(inv.EmecefNim)),
				prefixed("Compteurs : ", deref(inv.EmecefCounters))} {
				if l != "" {
					p.SetX(50)
					p.CellFormat(0, 4.5, tr(l), "", 2, "L", false, 0, "")
				}
			}
			if inv.EmecefCertifiedAt != nil {
				p.SetX(50)
				p.CellFormat(0, 4.5, tr("Certifiée le "+inv.EmecefCertifiedAt.In(localTZ).Format("02/01/2006 à 15h04:05")), "", 2, "L", false, 0, "")
			}
			afterTotal += 34
		}
	}

	// Mentions
	p.SetY(afterTotal)
	p.SetTextColor(mutedR, mutedG, mutedB)
	p.SetFont("Helvetica", "", 8)
	if inv.Footer != "" {
		p.MultiCell(0, 4.2, tr(inv.Footer), "", "L", false)
	}
	if inv.Kind == "invoice" {
		p.MultiCell(0, 4.2, tr("Facture acquittée. Billets et conditions de voyage : espace client ou agence."), "", "L", false)
	}

	var buf bytes.Buffer
	err := p.Output(&buf)
	return buf.Bytes(), err
}

// RenderTicket : facture au format ticket 80 mm (imprimante thermique du guichet).
func RenderTicket(inv store.Invoice, lines []store.InvoiceLine, original *store.Invoice) ([]byte, error) {
	height := 95.0 + float64(len(lines))*13
	if inv.EmecefStatus == "certified" {
		height += 45
	}
	d := newDoc(fpdf.SizeType{Wd: 80, Ht: height}, 4)
	p, tr := d.pdf, d.tr
	p.SetAutoPageBreak(false, 0)
	p.AddPage()
	w := 72.0
	center := func(size float64, style, text string, h float64) {
		p.SetFont("Helvetica", style, size)
		p.MultiCell(w, h, tr(text), "", "C", false)
	}
	p.SetTextColor(0, 0, 0)
	center(12, "B", inv.SellerName, 5.5)
	for _, l := range []string{inv.SellerLegalName, inv.SellerAddress,
		joinNonEmpty(" · ", prefixed("IFU ", inv.SellerTaxID), prefixed("RCCM ", inv.SellerTradeRegister)), inv.SellerPhone} {
		if strings.TrimSpace(l) != "" {
			center(7, "", l, 3.4)
		}
	}
	p.Ln(2)
	center(10, "B", title(inv)+" "+inv.Number, 5)
	center(7.5, "", inv.IssuedAt.In(localTZ).Format("02/01/2006 15h04")+prefixed(" · ", inv.Reference), 3.6)
	if original != nil {
		center(7, "", "Avoir sur la facture "+original.Number, 3.4)
	}
	dash := func() {
		p.SetFont("Courier", "", 8)
		p.CellFormat(w, 3.5, strings.Repeat("-", 42), "", 1, "C", false, 0, "")
	}
	dash()
	p.SetFont("Helvetica", "", 7.5)
	customer := inv.CustomerName
	if inv.CustomerCompany != "" {
		customer = inv.CustomerCompany + prefixed(" (IFU ", inv.CustomerTaxID)
		if inv.CustomerTaxID != "" {
			customer += ")"
		}
	}
	p.MultiCell(w, 3.6, tr("Client : "+customer+prefixed(" · ", inv.CustomerPhone)), "", "L", false)
	dash()
	for _, l := range lines {
		p.SetFont("Helvetica", "B", 7.5)
		p.MultiCell(w, 3.6, tr(fmt.Sprintf("%s", l.Label)), "", "L", false)
		if l.Detail != "" {
			p.SetFont("Helvetica", "", 6.8)
			p.MultiCell(w, 3.2, tr(l.Detail), "", "L", false)
		}
		p.SetFont("Helvetica", "", 7.5)
		p.CellFormat(w/2, 4, tr(fmt.Sprintf("%d x %s", l.Quantity, money(l.UnitPrice, inv.Currency))), "", 0, "L", false, 0, "")
		p.SetFont("Helvetica", "B", 8)
		p.CellFormat(w/2, 4, tr(money(l.Amount, inv.Currency)), "", 1, "R", false, 0, "")
	}
	dash()
	p.SetFont("Helvetica", "B", 10)
	label := "TOTAL PAYÉ"
	if inv.Kind == "credit_note" {
		label = "REMBOURSÉ"
	}
	p.CellFormat(w/2, 6, tr(label), "", 0, "L", false, 0, "")
	p.CellFormat(w/2, 6, tr(money(inv.TotalAmount, inv.Currency)), "", 1, "R", false, 0, "")
	p.SetFont("Helvetica", "", 7.5)
	p.CellFormat(w, 3.8, tr(inv.PaymentMethod+prefixed(" · ", inv.AgencyName)+prefixed(" · ", inv.OperatorName)), "", 1, "L", false, 0, "")
	if inv.EmecefStatus == "certified" && inv.EmecefQr != nil {
		if png, err := qrcode.Encode(*inv.EmecefQr, qrcode.Medium, 300); err == nil {
			p.Ln(2)
			p.RegisterImageOptionsReader("dgi", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(png))
			p.ImageOptions("dgi", 24, p.GetY(), 32, 32, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
			p.SetY(p.GetY() + 33)
			center(7, "B", "Facture normalisée DGI", 3.4)
			center(6.8, "", prefixed("Code MECeF : ", deref(inv.EmecefCode)), 3.2)
			center(6.8, "", prefixed("NIM : ", deref(inv.EmecefNim)), 3.2)
		}
	}
	if inv.Footer != "" {
		p.Ln(1)
		center(6.5, "", inv.Footer, 3)
	}
	p.Ln(1)
	center(7, "B", "Merci et bon voyage !", 3.6)
	var buf bytes.Buffer
	err := p.Output(&buf)
	return buf.Bytes(), err
}

func prefixed(prefix, v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return prefix + v
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
