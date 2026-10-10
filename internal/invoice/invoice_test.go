package invoice

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"busvoyage/internal/store"
)

func TestMoney(t *testing.T) {
	for v, want := range map[int64]string{0: "0 F CFA", 950: "950 F CFA", 25000: "25 000 F CFA", 1250000: "1 250 000 F CFA", -2000: "- 2 000 F CFA"} {
		if got := money(v, "XOF"); got != want {
			t.Errorf("money(%d) = %q, attendu %q", v, got, want)
		}
	}
}

func TestBookingLines(t *testing.T) {
	d := store.GetDepartureDetailRow{OriginCity: "Cotonou", DestinationCity: "Parakou", DepartureAt: time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)}
	seats := []store.ListBookingSeatsRow{
		{SeatLabel: "1A", PassengerName: "Awa", CategoryName: "VIP", TotalPrice: 12500, Status: "sold"},
		{SeatLabel: "1B", PassengerName: "Koffi", CategoryName: "VIP", TotalPrice: 12500, Status: "sold"},
		{SeatLabel: "2A", PassengerName: "Annulé", TotalPrice: 7500, Status: "cancelled"},
	}
	// Réservation simple avec points de fidélité : 25 000 − 2 000 = 23 000
	b := store.Booking{Reference: "BV1", TotalAmount: 23000, LoyaltyPointsUsed: 400, LoyaltyPointsAmount: 2000}
	lines := bookingLines(b, d, seats)
	if len(lines) != 3 || sum(lines) != 23000 || lines[2].Kind != "loyalty" {
		t.Fatalf("réservation : %+v", lines)
	}
	// Modification : 3 000 de différence + 2 000 de frais (2 passagers)
	id := uuid.New()
	c := store.Booking{Reference: "BV2", ChangedFromBookingID: &id, TotalAmount: 5000, ChangeFeeAmount: 2000}
	lines = bookingLines(c, d, seats)
	if len(lines) != 2 || sum(lines) != 5000 || lines[1].Quantity != 2 || lines[1].UnitPrice != 1000 {
		t.Fatalf("modification : %+v", lines)
	}
}

func sampleInvoice() (store.Invoice, []store.InvoiceLine) {
	inv := store.Invoice{Kind: "invoice", Number: "FAC-2026-000042", IssuedAt: time.Date(2026, 10, 8, 14, 5, 0, 0, localTZ),
		Reference: "BV7K3M9QXT", SellerName: "Trans Express", SellerLegalName: "Trans Express SARL", SellerTaxID: "3201234567890",
		SellerTradeRegister: "RB/COT/24 B 1234", SellerAddress: "Carrefour Jonquet, Cotonou", SellerPhone: "+229 01 00 00 00 00",
		SellerEmail: "contact@trans-express.bj", Footer: "TVA non applicable — à adapter selon le régime fiscal de la compagnie.",
		CustomerName: "Awa Dossou", CustomerPhone: "+2290197000010", CustomerEmail: "awa@exemple.bj",
		CustomerCompany: "Société Béninoise d'Import", CustomerTaxID: "3209876543210",
		AgencyName: "Cotonou — Jonquet", OperatorName: "Caissier Cotonou", PaymentMethod: "Espèces", Currency: "XOF", TotalAmount: 23000}
	lines := []store.InvoiceLine{
		{Kind: "ticket", Label: "Billet Cotonou – Parakou (VIP)", Detail: "09/10/2026 07h00 · siège 1A · Awa Dossou", Quantity: 1, UnitPrice: 12500, Amount: 12500},
		{Kind: "ticket", Label: "Billet Cotonou – Parakou (VIP)", Detail: "09/10/2026 07h00 · siège 1B · Koffi Dossou", Quantity: 1, UnitPrice: 12500, Amount: 12500},
		{Kind: "loyalty", Label: "Points de fidélité utilisés", Detail: "400 points", Quantity: 1, UnitPrice: -2000, Amount: -2000},
	}
	return inv, lines
}

func TestRenderPDF(t *testing.T) {
	inv, lines := sampleInvoice()
	a4, err := RenderA4(inv, lines, nil)
	if err != nil || !bytes.HasPrefix(a4, []byte("%PDF")) {
		t.Fatalf("A4 : %v", err)
	}
	ticket, err := RenderTicket(inv, lines, nil)
	if err != nil || !bytes.HasPrefix(ticket, []byte("%PDF")) {
		t.Fatalf("ticket : %v", err)
	}
	// Version certifiée (aperçu de la phase e-MECeF) et avoir.
	code, qr, nim := "TEST-ABCD-1234-EFGH-5678-IJKL", "F;TS01234567;TESTABCD1234EFGH5678IJKL;3201234567890;20261008140501", "TS01234567"
	certified := inv
	certified.EmecefStatus, certified.EmecefCode, certified.EmecefQr, certified.EmecefNim = "certified", &code, &qr, &nim
	cert, err := RenderA4(certified, lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	credit := inv
	credit.Kind, credit.Number, credit.TotalAmount = "credit_note", "AV-2026-000003", 20700
	cn, err := RenderA4(credit, []store.InvoiceLine{{Kind: "refund", Label: "Remboursement — réservation BV7K3M9QXT", Detail: "Annulée par le client", Quantity: 1, UnitPrice: 20700, Amount: 20700}}, &inv)
	if err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("BV_DUMP_DIR"); dir != "" {
		for name, b := range map[string][]byte{"facture-a4.pdf": a4, "facture-ticket.pdf": ticket, "facture-certifiee.pdf": cert, "avoir.pdf": cn} {
			_ = os.WriteFile(filepath.Join(dir, name), b, 0o644)
		}
	}
}
