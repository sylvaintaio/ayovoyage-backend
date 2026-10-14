package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"busvoyage/internal/jobs"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/push"
	"busvoyage/internal/segment"
	"busvoyage/internal/store"
)

// Note : notification destinée à un client (app Voyageur) ou à un membre du personnel (app Agent).
// Elle est rangée dans sa boîte de réception, puis poussée sur ses téléphones par SendPushWorker.
type Note struct {
	CustomerID *uuid.UUID
	StaffID    *uuid.UUID
	CompanyID  *uuid.UUID
	Kind       string
	Title      string
	Body       string
	// Link : écran de l'application à ouvrir (ex. /reservations/<id>).
	Link string
	Data map[string]string
	// DedupKey : un même événement n'est notifié qu'une fois à une même personne.
	DedupKey string
}

// Inbox range la notification ; sans effet si elle a déjà été envoyée (DedupKey) ou sans destinataire.
func (n *Notifier) Inbox(ctx context.Context, q *store.Queries, note Note) error {
	if note.CustomerID == nil && note.StaffID == nil {
		return nil
	}
	data, err := json.Marshal(note.Data)
	if err != nil || note.Data == nil {
		data = []byte("{}")
	}
	var dedup *string
	if note.DedupKey != "" {
		dedup = &note.DedupKey
	}
	companyID := note.CompanyID
	if note.CustomerID != nil {
		companyID = nil // compagnie d'origine seulement pour le personnel (cloisonnement)
	}
	_, err = q.InsertInboxMessage(ctx, store.InsertInboxMessageParams{CustomerID: note.CustomerID, StaffID: note.StaffID,
		CompanyID: companyID, Kind: note.Kind, Title: note.Title, Body: note.Body, Link: note.Link, Data: data, DedupKey: dedup})
	if database.IsNotFound(err) { // déjà notifié (ON CONFLICT DO NOTHING)
		return nil
	}
	return err
}

// inboxQuiet : comme Inbox, une erreur est seulement journalisée (le SMS ou l'e-mail reste prioritaire).
func (n *Notifier) inboxQuiet(ctx context.Context, q *store.Queries, note Note) {
	if err := n.Inbox(ctx, q, note); err != nil {
		slog.Error("notification (boîte de réception)", "kind", note.Kind, "err", err)
	}
}

// ---------------------------------------------------------------- Envoi sur les téléphones

// SendPushWorker pousse les notifications en attente (tâche périodique, quelques secondes de délai).
// Un échec réseau est retenté (5 fois au plus) ; un jeton refusé par Firebase est oublié.
type SendPushWorker struct {
	river.WorkerDefaults[jobs.SendPushArgs]
	N *Notifier
}

func (w *SendPushWorker) Work(ctx context.Context, _ *river.Job[jobs.SendPushArgs]) error {
	n := w.N
	if n.Push == nil {
		return nil
	}
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		msgs, err := q.ClaimInboxToPush(ctx, 50)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if err := n.pushOne(ctx, q, m); err != nil {
				slog.Warn("notification non envoyée, nouvelle tentative", "message", m.ID, "err", err)
				if err := q.MarkInboxPushFailed(ctx, m.ID); err != nil {
					return err
				}
				continue
			}
			if err := q.MarkInboxPushed(ctx, m.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (n *Notifier) pushOne(ctx context.Context, q *store.Queries, m store.InboxMessage) error {
	var devices []store.PushDevice
	var err error
	if m.CustomerID != nil {
		devices, err = q.ListPushDevicesForCustomer(ctx, m.CustomerID)
	} else {
		devices, err = q.ListPushDevicesForStaff(ctx, m.StaffID)
	}
	if err != nil || len(devices) == 0 {
		return err // aucun téléphone inscrit : le message reste dans la boîte de réception
	}
	unread, err := q.CountUnreadInbox(ctx, store.CountUnreadInboxParams{CustomerID: m.CustomerID, StaffID: m.StaffID})
	if err != nil {
		return err
	}
	data := map[string]string{}
	_ = json.Unmarshal(m.Data, &data)
	data["inbox_id"], data["kind"], data["link"] = m.ID.String(), m.Kind, m.Link

	var lastErr error
	sent := 0
	for _, d := range devices {
		err := n.Push.Send(ctx, push.Message{Token: d.Token, Title: m.Title, Body: m.Body, Data: data, Badge: int(unread)})
		switch {
		case err == nil:
			sent++
		case errors.Is(err, push.ErrUnregistered):
			if err := q.DeletePushDeviceByToken(ctx, d.Token); err != nil {
				return err
			}
		default:
			lastErr = err
		}
	}
	if m.CustomerID != nil {
		status, errMsg := DeliveryStatus(n.Push, lastErr)
		_ = q.InsertNotification(ctx, store.InsertNotificationParams{CompanyID: nil, CustomerID: m.CustomerID, Channel: "push",
			Template: m.Kind, Recipient: fmt.Sprintf("%d téléphone(s)", len(devices)), Status: status, Error: errMsg, Body: m.Body})
	}
	if sent == 0 {
		return lastErr // tous en échec réseau : nouvelle tentative
	}
	return nil
}

// ---------------------------------------------------------------- Avancement du car

// DepartureProgressWorker : le car a quitté un arrêt. Préviens les voyageurs qui attendent plus loin :
// au départ du premier arrêt, tous ; ensuite, ceux du prochain arrêt où quelqu'un attend.
// L'heure de passage tient compte du retard constaté.
type DepartureProgressWorker struct {
	river.WorkerDefaults[jobs.DepartureProgressArgs]
	N *Notifier
}

func (w *DepartureProgressWorker) Work(ctx context.Context, job *river.Job[jobs.DepartureProgressArgs]) error {
	n := w.N
	a := job.Args
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		d, err := q.GetDepartureDetail(ctx, a.DepartureID)
		if err != nil {
			return err
		}
		stops, err := segment.LoadStops(ctx, q, a.DepartureID)
		if err != nil || a.Seq < 0 || a.Seq >= len(stops) {
			return err
		}
		left := stops[a.Seq]
		// Retard au départ de cet arrêt (heure prévue + pause) ; une avance n'est pas annoncée.
		delay := max(a.DepartedAt.Sub(left.ScheduledAt.Add(time.Duration(left.PauseMinutes)*time.Minute)), 0)
		waiting, err := q.ListWaitingTravellers(ctx, store.ListWaitingTravellersParams{DepartureID: a.DepartureID, AfterSeq: int32(a.Seq)})
		if err != nil {
			return err
		}
		next := -1
		for _, t := range waiting {
			if next < 0 || int(t.BoardingSeq) < next {
				next = int(t.BoardingSeq)
			}
		}
		for _, t := range waiting {
			if a.Seq > 0 && int(t.BoardingSeq) != next {
				continue
			}
			stop := stops[t.BoardingSeq]
			eta := stop.ScheduledAt.Add(delay)
			body := fmt.Sprintf("Votre car %s a quitté %s à %s. Passage prévu à %s vers %s.",
				d.RouteName, left.Name, clock(a.DepartedAt), stop.Name, clock(eta))
			if delay >= 10*time.Minute {
				body += fmt.Sprintf(" Retard d'environ %d min.", int(delay.Minutes()))
			}
			companyID := d.CompanyID
			n.inboxQuiet(ctx, q, Note{CustomerID: t.CustomerID, CompanyID: &companyID, Kind: "departure_progress",
				Title: "Votre car arrive", Body: body, Link: "/reservations/" + t.BookingID.String(),
				Data:     map[string]string{"booking_id": t.BookingID.String(), "departure_id": d.ID.String()},
				DedupKey: fmt.Sprintf("progress:%s:%d", d.ID, a.Seq)})
		}
		return nil
	})
}

// ---------------------------------------------------------------- Équipage

// CrewAssignedWorker : un membre du personnel est affecté à un départ.
type CrewAssignedWorker struct {
	river.WorkerDefaults[jobs.CrewAssignedArgs]
	N *Notifier
}

var crewRoles = map[string]string{"controller": "contrôleur", "driver": "chauffeur", "assistant": "équipage"}

func (w *CrewAssignedWorker) Work(ctx context.Context, job *river.Job[jobs.CrewAssignedArgs]) error {
	n := w.N
	return n.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		d, err := q.GetDepartureDetail(ctx, job.Args.DepartureID)
		if err != nil {
			return err
		}
		crew, err := q.ListDepartureStaffIDs(ctx, d.ID)
		if err != nil {
			return err
		}
		role := map[uuid.UUID]string{}
		for _, c := range crew {
			role[c.StaffID] = crewRoles[c.Role]
		}
		for _, id := range job.Args.StaffIDs {
			r, ok := role[id]
			if !ok {
				continue // retiré entre-temps
			}
			staffID, companyID := id, d.CompanyID
			n.inboxQuiet(ctx, q, Note{StaffID: &staffID, CompanyID: &companyID, Kind: "crew_assigned",
				Title: "Nouveau départ", Body: fmt.Sprintf("Vous êtes %s sur %s, le %s.", r, d.RouteName, formatDate(d.DepartureAt)),
				Link: "/departs/" + d.ID.String(), Data: map[string]string{"departure_id": d.ID.String()},
				DedupKey: fmt.Sprintf("crew:%s:%s", d.ID, d.DepartureAt.Format(time.RFC3339))})
		}
		return nil
	})
}

func clock(t time.Time) string {
	return t.In(localTZ).Format("15h04")
}
