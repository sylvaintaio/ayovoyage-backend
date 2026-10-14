package ticket

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Logo de la compagnie (Paramètres → logo_url), imprimé sur les billets.
// Téléchargé par le serveur : HTTPS seulement, aucune adresse interne (réseau local, serveur lui-même),
// 1 Mo au plus, PNG ou JPEG ; gardé en mémoire une heure. Un logo inaccessible n'empêche pas le billet.

type logoImage struct {
	data []byte
	kind string // PNG ou JPG (fpdf)
}

type logoEntry struct {
	img     *logoImage
	expires time.Time
}

var (
	logoMu    sync.Mutex
	logoCache = map[string]logoEntry{}
)

const (
	logoMaxBytes = 1 << 20
	logoTTL      = time.Hour
	logoFailTTL  = 10 * time.Minute
)

var errUnsafeAddress = errors.New("adresse non autorisée")

// safeDialer refuse les adresses privées, locales ou réservées (protection contre les requêtes
// vers le réseau interne via une URL saisie dans les paramètres).
var safeDialer = &net.Dialer{
	Timeout: 5 * time.Second,
	Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
			return errUnsafeAddress
		}
		return nil
	},
}

var logoClient = &http.Client{
	Timeout: 12 * time.Second,
	Transport: &http.Transport{DialContext: safeDialer.DialContext, TLSHandshakeTimeout: 8 * time.Second,
		ResponseHeaderTimeout: 6 * time.Second},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Scheme != "https" {
			return errUnsafeAddress
		}
		return nil
	},
}

// companyLogo : logo prêt pour le PDF, ou nil (pas de logo, inaccessible, format non pris en charge).
func companyLogo(ctx context.Context, url *string) *logoImage {
	if url == nil || !strings.HasPrefix(strings.TrimSpace(*url), "https://") {
		return nil
	}
	u := strings.TrimSpace(*url)
	logoMu.Lock()
	if e, ok := logoCache[u]; ok && time.Now().Before(e.expires) {
		logoMu.Unlock()
		return e.img
	}
	logoMu.Unlock()

	img := fetchLogo(ctx, u)
	ttl := logoTTL
	if img == nil {
		ttl = logoFailTTL
	}
	logoMu.Lock()
	logoCache[u] = logoEntry{img: img, expires: time.Now().Add(ttl)}
	logoMu.Unlock()
	return img
}

func fetchLogo(ctx context.Context, url string) *logoImage {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	res, err := logoClient.Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, logoMaxBytes+1))
	if err != nil || len(data) > logoMaxBytes {
		return nil
	}
	switch http.DetectContentType(data) {
	case "image/png":
		return &logoImage{data: data, kind: "PNG"}
	case "image/jpeg":
		return &logoImage{data: data, kind: "JPG"}
	}
	return nil
}
