// Package push envoie les notifications sur les téléphones (Firebase Cloud Messaging,
// qui relaie aussi vers les iPhone via APNs).
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ErrUnregistered : jeton refusé par Firebase (application désinstallée, jeton périmé) : à oublier.
var ErrUnregistered = errors.New("jeton de notification inconnu ou périmé")

// Message : notification pour un téléphone.
type Message struct {
	Token string
	Title string
	Body  string
	// Data : lu par l'application à l'ouverture (écran à afficher, identifiant du message).
	Data map[string]string
	// Badge : nombre de messages non lus, affiché sur l'icône de l'application.
	Badge int
}

// Sender envoie une notification.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Log : pilote de développement, la notification est écrite dans le journal.
type Log struct{}

func (Log) Send(_ context.Context, m Message) error {
	slog.Info("notification (simulée)", "title", m.Title, "body", m.Body, "badge", m.Badge, "data", m.Data)
	return nil
}

// FCM : Firebase Cloud Messaging, API HTTP v1.
type FCM struct {
	ProjectID string
	tokens    oauth2.TokenSource
	http      *http.Client
}

// NewFromFile : pilote FCM depuis la clé de compte de service (fichier JSON).
// Chemin vide : pilote Log (aucune notification réellement envoyée).
func NewFromFile(ctx context.Context, path string) (Sender, error) {
	if strings.TrimSpace(path) == "" {
		return Log{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("clé Firebase illisible (FIREBASE_CREDENTIALS_FILE) : %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, raw, "https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return nil, fmt.Errorf("clé Firebase invalide : %w", err)
	}
	if creds.ProjectID == "" {
		return nil, errors.New("clé Firebase sans project_id")
	}
	return &FCM{ProjectID: creds.ProjectID, tokens: creds.TokenSource, http: &http.Client{Timeout: 10 * time.Second}}, nil
}

func (f *FCM) Send(ctx context.Context, m Message) error {
	tok, err := f.tokens.Token()
	if err != nil {
		return fmt.Errorf("jeton d'accès Firebase : %w", err)
	}
	badge := m.Badge
	payload := map[string]any{"message": map[string]any{
		"token":        m.Token,
		"notification": map[string]string{"title": m.Title, "body": m.Body},
		"data":         m.Data,
		"android": map[string]any{
			"priority": "high",
			"notification": map[string]any{
				"channel_id":         "ayo_voyage",
				"sound":              "default",
				"notification_count": badge,
			},
		},
		"apns": map[string]any{
			"payload": map[string]any{"aps": map[string]any{"badge": badge, "sound": "default"}},
		},
	}}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := "https://fcm.googleapis.com/v1/projects/" + f.ProjectID + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	res, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if unregistered(res.StatusCode, raw) {
		return ErrUnregistered
	}
	return fmt.Errorf("FCM %d : %s", res.StatusCode, strings.TrimSpace(string(raw)))
}

// unregistered : réponses FCM qui signifient que le jeton ne servira plus.
func unregistered(status int, body []byte) bool {
	if status == http.StatusNotFound {
		return true
	}
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	for _, d := range e.Error.Details {
		if d.ErrorCode == "UNREGISTERED" {
			return true
		}
	}
	return status == http.StatusBadRequest && e.Error.Status == "INVALID_ARGUMENT" && strings.Contains(string(body), "registration token")
}
