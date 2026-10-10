// Package jobs définit les tâches de fond (River, stockées dans PostgreSQL).
// Insérer une tâche dans la même transaction que la donnée métier garantit
// qu'elle n'est exécutée que si la transaction est validée (outbox transactionnelle).
package jobs

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

type BookingConfirmedArgs struct {
	BookingID uuid.UUID `json:"booking_id"`
}

func (BookingConfirmedArgs) Kind() string { return "notify_booking_confirmed" }

// WelcomeArgs : e-mail de bienvenue après la création d'un compte client.
type WelcomeArgs struct {
	CustomerID uuid.UUID `json:"customer_id"`
}

func (WelcomeArgs) Kind() string { return "notify_welcome" }

// PayLaterNoticeArgs : message « payer en agence » (réservée, expirée).
type PayLaterNoticeArgs struct {
	BookingID uuid.UUID `json:"booking_id"`
	Event     string    `json:"event"` // reserved | expired
}

func (PayLaterNoticeArgs) Kind() string { return "notify_pay_later" }

// SendPayLaterRemindersArgs : rappels avant l'échéance de paiement (tâche périodique).
type SendPayLaterRemindersArgs struct{}

func (SendPayLaterRemindersArgs) Kind() string { return "send_pay_later_reminders" }

type BookingCancelledArgs struct {
	BookingID uuid.UUID `json:"booking_id"`
}

func (BookingCancelledArgs) Kind() string { return "notify_booking_cancelled" }

type DepartureStatusArgs struct {
	DepartureID uuid.UUID `json:"departure_id"`
	Status      string    `json:"status"`
}

func (DepartureStatusArgs) Kind() string { return "notify_departure_status" }

type LoyaltyEarnArgs struct {
	TicketID uuid.UUID `json:"ticket_id"`
}

func (LoyaltyEarnArgs) Kind() string { return "loyalty_earn" }

// SendRemindersArgs : rappels « la veille du voyage » (tâche horaire).
type SendRemindersArgs struct{}

func (SendRemindersArgs) Kind() string { return "send_trip_reminders" }

// SendReviewInvitationsArgs : invitations à donner son avis après le voyage (tâche horaire).
type SendReviewInvitationsArgs struct{}

func (SendReviewInvitationsArgs) Kind() string { return "send_review_invitations" }

// PurgeDataArgs : purge des codes de connexion et jetons expirés (tâche quotidienne).
type PurgeDataArgs struct{}

func (PurgeDataArgs) Kind() string { return "purge_expired_data" }

// SendInvoiceEmailsArgs : envoi des factures en attente (tâche chaque minute).
type SendInvoiceEmailsArgs struct{}

func (SendInvoiceEmailsArgs) Kind() string { return "send_invoice_emails" }

type ExpireBookingsArgs struct{}

func (ExpireBookingsArgs) Kind() string { return "expire_bookings" }

type ExpireLoyaltyPointsArgs struct{}

func (ExpireLoyaltyPointsArgs) Kind() string { return "expire_loyalty_points" }

// Queue permet aux services d'insérer des tâches ; le client River est
// injecté après sa création (les workers doivent exister avant lui).
type Queue struct {
	client *river.Client[pgx.Tx]
}

func (q *Queue) SetClient(c *river.Client[pgx.Tx]) { q.client = c }

func (q *Queue) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs) error {
	return q.InsertTxOpts(ctx, tx, args, nil)
}

// InsertTxOpts : comme InsertTx, avec options (exécution différée, unicité…).
func (q *Queue) InsertTxOpts(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) error {
	if q.client == nil {
		return errors.New("jobs: client River non initialisé")
	}
	_, err := q.client.InsertTx(ctx, tx, args, opts)
	return err
}
