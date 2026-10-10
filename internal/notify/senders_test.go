package notify

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestBuildMessageWithAttachment(t *testing.T) {
	from := &mail.Address{Name: "Trans Express", Address: "factures@exemple.bj"}
	to := &mail.Address{Address: "awa@exemple.bj"}
	pdf := bytes.Repeat([]byte("%PDF-1.4 facture "), 40)
	raw, err := buildMessage(from, to, Email{
		Subject: "Facture FAC-2026-000001", Text: "Bonjour", HTML: "<p>Bonjour</p>",
		Attachments: []Attachment{{Name: "FAC-2026-000001.pdf", ContentType: "application/pdf", Data: pdf}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("ligne trop longue pour SMTP (%d)", len(line))
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	typ, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if typ != "multipart/mixed" {
		t.Fatalf("Content-Type = %s", typ)
	}
	r := multipart.NewReader(msg.Body, params["boundary"])
	body, err := r.NextPart()
	if err != nil || !strings.HasPrefix(body.Header.Get("Content-Type"), "multipart/alternative") {
		t.Fatalf("1re partie : %v %q", err, body.Header.Get("Content-Type"))
	}
	att, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if att.FileName() != "FAC-2026-000001.pdf" {
		t.Errorf("nom de fichier %q", att.FileName())
	}
	got, _ := io.ReadAll(att) // mime/multipart ne décode que le quoted-printable
	if att.Header.Get("Content-Transfer-Encoding") != "base64" {
		t.Fatal("pièce jointe non encodée en base64")
	}
	dec, err := io.ReadAll(base64Reader(got))
	if err != nil || !bytes.Equal(dec, pdf) {
		t.Fatalf("contenu de la pièce jointe altéré (%v)", err)
	}
	if _, err := r.NextPart(); err != io.EOF {
		t.Fatalf("partie inattendue : %v", err)
	}
}

func TestBuildMessageWithoutAttachmentUnchanged(t *testing.T) {
	raw, err := buildMessage(&mail.Address{Address: "a@exemple.bj"}, &mail.Address{Address: "b@exemple.bj"}, Email{Subject: "S", Text: "T"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Content-Type: text/plain; charset=UTF-8") || strings.Contains(string(raw), "multipart/mixed") {
		t.Fatal("un e-mail sans pièce jointe doit rester en texte simple")
	}
}

func TestInvoiceEmail(t *testing.T) {
	at := time.Date(2026, 10, 8, 21, 5, 0, 0, time.UTC)
	m := InvoiceEmail(Brand{Name: "Trans Express"}, "awa@exemple.bj", InvoiceView{
		CustomerName: "Awa <b>", Number: "AV-2026-000001", CreditNote: true, Reference: "BV7K3M9QXT",
		Amount: 6750, Currency: "XOF", IssuedAt: at, Link: "https://exemple.bj/compte/factures",
	})
	if m.Subject != "Avoir AV-2026-000001 — Trans Express" {
		t.Errorf("sujet %q", m.Subject)
	}
	for _, want := range []string{FormatMoney(6750, "XOF"), "08/10/2026 à 22h05", "Awa &lt;b&gt;", "Mes factures"} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("HTML sans %q", want)
		}
	}
	if !strings.Contains(m.Text, "remboursement a été versé") {
		t.Error("texte d'avoir attendu")
	}
	dump(t, "facture.html", m.HTML)
}

func base64Reader(b []byte) io.Reader {
	return base64.NewDecoder(base64.StdEncoding, bytes.NewReader(bytes.ReplaceAll(b, []byte("\r\n"), nil)))
}
