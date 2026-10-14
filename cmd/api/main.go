// Commande principale : un seul binaire pour servir l'API, migrer, initialiser
// les données de démonstration et exporter la spécification OpenAPI.
//
//	api serve      démarre l'API et les tâches de fond (par défaut)
//	api migrate    applique les migrations (MIGRATION_DATABASE_URL)
//	api seed       crée les données de démonstration (développement)
//	api openapi    écrit api/openapi.yaml et api/openapi-3.0.yaml
//	api ticket-key génère une nouvelle clé de signature des billets (rotation)
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"busvoyage/internal/admin"
	"busvoyage/internal/auth"
	"busvoyage/internal/boarding"
	"busvoyage/internal/booking"
	"busvoyage/internal/config"
	"busvoyage/internal/departure"
	"busvoyage/internal/fleet"
	"busvoyage/internal/identity"
	"busvoyage/internal/inbox"
	"busvoyage/internal/invoice"
	"busvoyage/internal/jobs"
	"busvoyage/internal/loyalty"
	"busvoyage/internal/network"
	"busvoyage/internal/notify"
	"busvoyage/internal/parcel"
	"busvoyage/internal/payment"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/migrate"
	"busvoyage/internal/pricing"
	"busvoyage/internal/push"
	"busvoyage/internal/review"
	"busvoyage/internal/seed"
	"busvoyage/internal/server"
	"busvoyage/internal/ticket"
	"busvoyage/internal/ticketsign"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cmd); err != nil {
		slog.Error("arrêt sur erreur", "commande", cmd, "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string) error {
	cfg, err := config.Load()
	if err != nil && cmd != "openapi" && cmd != "ticket-key" {
		return err
	}
	switch cmd {
	case "migrate":
		if cfg.MigrationDatabaseURL == "" {
			return errors.New("MIGRATION_DATABASE_URL est obligatoire pour migrer")
		}
		return migrate.Up(ctx, cfg.MigrationDatabaseURL)
	case "seed":
		if !cfg.AllowsSimulation() {
			return errors.New("le jeu de démonstration est interdit en production")
		}
		db, err := database.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer db.Pool.Close()
		return seed.Run(ctx, db)
	case "openapi":
		return exportOpenAPI(cfg)
	case "ticket-key":
		return printTicketKey(cfg)
	case "sms-test":
		return smsTest(ctx, cfg)
	case "create-company":
		return createCompany(ctx, cfg, os.Args[2:], os.Stdout)
	case "serve":
		return serve(ctx, cfg)
	default:
		return fmt.Errorf("commande inconnue %q (serve, migrate, seed, create-company, openapi, ticket-key, sms-test)", cmd)
	}
}

// app regroupe les dépendances et modules HTTP.
type app struct {
	handler http.Handler
	river   *river.Client[pgx.Tx]
}

func build(ctx context.Context, cfg config.Config, db *database.DB) (*app, func() ([]byte, []byte, error), error) {
	signer, ephemeral, err := ticketsign.New(cfg.TicketSigningKeyID, cfg.TicketSigningKey)
	if err != nil {
		return nil, nil, err
	}
	if ephemeral {
		slog.Warn("TICKET_SIGNING_KEY absente : clé éphémère générée (les billets deviendront invalides au redémarrage)")
	}
	for kid, pub := range cfg.TicketVerifyKeys { // rotation : anciennes clés encore acceptées
		if err := signer.AcceptKey(kid, pub); err != nil {
			return nil, nil, fmt.Errorf("TICKET_VERIFY_KEYS (%s) : %w", kid, err)
		}
	}
	sms, email, err := notify.NewSenders(cfg)
	if err != nil {
		return nil, nil, err
	}
	issuer := auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTokenTTL)
	queue := &jobs.Queue{}
	mailer := &invoice.Mailer{DB: db} // notificateur branché plus bas

	tickets := &ticket.Service{DB: db, Signer: signer}
	bookings := &booking.Service{DB: db, Cfg: cfg, Tickets: tickets, Queue: queue}
	parcels := &parcel.Service{DB: db, Cfg: cfg, SMS: sms, Email: email}
	departures := &departure.Handler{DB: db, Bookings: bookings, Queue: queue, Parcels: parcels}

	providers := map[string]payment.Provider{}
	for _, name := range cfg.PaymentProviders {
		switch name {
		case "mock":
			providers[name] = payment.Mock{APIURL: cfg.PublicAPIURL}
		case "fedapay":
			providers[name] = payment.NewFedaPay(cfg.FedaPaySecretKey, cfg.FedaPayWebhookSecret, cfg.FedaPayEnv)
		default:
			return nil, nil, fmt.Errorf("fournisseur de paiement non pris en charge : %s", name)
		}
	}

	handler, api := server.New(cfg, issuer, func() error {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if db == nil {
			return nil
		}
		return db.Pool.Ping(c)
	},
		&identity.Handler{S: &identity.Service{DB: db, Cfg: cfg, Tokens: issuer, SMS: sms, Email: email, Queue: queue}},
		&network.Handler{DB: db},
		&fleet.Handler{DB: db},
		departures,
		bookings,
		&payment.Service{DB: db, Cfg: cfg, Bookings: bookings, Providers: providers},
		tickets,
		&boarding.Handler{DB: db, Signer: signer, Queue: queue, Departures: departures, Bookings: bookings},
		&loyalty.Handler{DB: db, Cfg: cfg},
		&admin.Handler{DB: db},
		parcels,
		&invoice.Handler{DB: db, Mailer: mailer},
		&pricing.Handler{DB: db, Cfg: cfg},
		&review.Handler{DB: db},
		&inbox.Handler{DB: db},
	)

	export := func() ([]byte, []byte, error) {
		y31, err := api.OpenAPI().YAML()
		if err != nil {
			return nil, nil, err
		}
		y30, err := api.OpenAPI().DowngradeYAML()
		return y31, y30, err
	}
	if db == nil {
		return &app{handler: handler}, export, nil
	}

	pushSender, err := push.NewFromFile(context.Background(), cfg.FirebaseCredentialsFile)
	if err != nil {
		return nil, nil, err
	}
	notifier := &notify.Notifier{Brand: notify.BrandFromConfig(cfg), DB: db, SMS: sms, Email: email, Push: pushSender,
		WebURL: cfg.PublicWebURL, BookingInvoices: invoice.BookingInvoices}
	mailer.N = notifier
	workers := river.NewWorkers()
	river.AddWorker(workers, &notify.WelcomeWorker{N: notifier})
	river.AddWorker(workers, &notify.BookingConfirmedWorker{N: notifier})
	river.AddWorker(workers, &notify.BookingCancelledWorker{N: notifier})
	river.AddWorker(workers, &notify.DepartureStatusWorker{N: notifier})
	river.AddWorker(workers, &notify.RemindersWorker{N: notifier})
	river.AddWorker(workers, &notify.PurgeWorker{DB: db})
	river.AddWorker(workers, &loyalty.EarnWorker{DB: db})
	river.AddWorker(workers, &loyalty.ExpireWorker{DB: db})
	river.AddWorker(workers, &booking.ExpireWorker{DB: db, Queue: queue})
	river.AddWorker(workers, &invoice.EmailWorker{M: mailer})
	river.AddWorker(workers, &notify.PayLaterWorker{N: notifier})
	river.AddWorker(workers, &notify.PayLaterRemindersWorker{N: notifier})
	river.AddWorker(workers, &notify.ReviewInvitationsWorker{N: notifier})
	river.AddWorker(workers, &notify.SendPushWorker{N: notifier})
	river.AddWorker(workers, &notify.DepartureProgressWorker{N: notifier})
	river.AddWorker(workers, &notify.CrewAssignedWorker{N: notifier})

	rc, err := river.NewClient(riverpgxv5.New(db.Pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 10}},
		Workers: workers,
		Logger:  slog.Default(),
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.ExpireBookingsArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.ExpireLoyaltyPointsArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			// Rappels « la veille » : toutes les heures (envoi entre 7 h et 21 h seulement).
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.SendRemindersArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			// Avis : invitations environ 2 h après l'arrivée.
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.SendReviewInvitationsArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			// Payer en agence : rappels avant l'échéance.
			river.NewPeriodicJob(river.PeriodicInterval(15*time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.SendPayLaterRemindersArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			// Notifications en attente : poussées sur les téléphones (délai de quelques secondes).
			river.NewPeriodicJob(river.PeriodicInterval(10*time.Second),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.SendPushArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			// Factures et avoirs à envoyer par e-mail (PDF joint).
			river.NewPeriodicJob(river.PeriodicInterval(time.Minute),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.SendInvoiceEmailsArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
			// Purge des codes de connexion et sessions expirés.
			river.NewPeriodicJob(river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.PurgeDataArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true}),
		},
	})
	if err != nil {
		return nil, nil, err
	}
	queue.SetClient(rc)
	return &app{handler: handler, river: rc}, export, nil
}

func serve(ctx context.Context, cfg config.Config) error {
	if cfg.AutoMigrate && cfg.MigrationDatabaseURL != "" {
		if err := migrate.Up(ctx, cfg.MigrationDatabaseURL); err != nil {
			return err
		}
	}
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connexion PostgreSQL: %w", err)
	}
	defer db.Pool.Close()

	a, _, err := build(ctx, cfg, db)
	if err != nil {
		return err
	}
	if err := a.river.Start(ctx); err != nil {
		return fmt.Errorf("démarrage des tâches de fond: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           a.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("API démarrée", "addr", cfg.HTTPAddr, "env", cfg.Env, "docs", cfg.PublicAPIURL+"/docs")
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	slog.Info("arrêt en cours…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return a.river.Stop(shutdownCtx)
}

// exportOpenAPI génère la spécification sans base de données (utile en CI).
func exportOpenAPI(cfg config.Config) error {
	if cfg.PublicAPIURL == "" {
		cfg.PublicAPIURL = "http://localhost:8080"
	}
	cfg.Env = "production" // n'expose pas les routes de développement
	if len(cfg.JWTSecret) == 0 {
		cfg.JWTSecret = make([]byte, 32)
	}
	_, export, err := build(context.Background(), cfg, nil)
	if err != nil {
		return err
	}
	y31, y30, err := export()
	if err != nil {
		return err
	}
	if err := os.MkdirAll("api", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join("api", "openapi.yaml"), y31, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join("api", "openapi-3.0.yaml"), y30, 0o644); err != nil {
		return err
	}
	slog.Info("spécification OpenAPI écrite", "fichiers", []string{"api/openapi.yaml", "api/openapi-3.0.yaml"})
	return nil
}

// printTicketKey prépare une rotation de la clé de signature des billets.
func printTicketKey(cfg config.Config) error {
	seed, pub, err := ticketsign.GenerateKey()
	if err != nil {
		return err
	}
	newID := "k" + time.Now().Format("20060102")
	fmt.Println("# Nouvelle clé de signature des billets — à reporter dans la configuration (.env / secrets).")
	fmt.Println("# La seed est secrète : ne la partagez pas, ne la commitez pas.")
	fmt.Printf("TICKET_SIGNING_KEY_ID=%s\n", newID)
	fmt.Printf("TICKET_SIGNING_KEY=%s\n", seed)
	fmt.Printf("# Clé publique (diffusée aux terminaux) : %s\n", pub)
	if len(cfg.TicketSigningKey) == 32 {
		old := cfg.TicketSigningKeyID + ":" + ticketsign.PublicKeyOf(cfg.TicketSigningKey)
		for kid, p := range cfg.TicketVerifyKeys {
			old += "," + kid + ":" + base64.StdEncoding.EncodeToString(p)
		}
		fmt.Println("# Garder l'ancienne clé acceptée jusqu'au dernier voyage des billets déjà émis :")
		fmt.Printf("TICKET_VERIFY_KEYS=%s\n", old)
	}
	return nil
}

// smsTest envoie un SMS de test avec la configuration courante :
//
//	api sms-test +22997000000 ["texte"]
//
// Affiche le texte réellement transmis et le nombre de SMS facturés.
func smsTest(ctx context.Context, cfg config.Config) error {
	if len(os.Args) < 3 {
		return errors.New("usage : api sms-test <numéro> [message]")
	}
	message := "Test d'envoi " + cfg.BrandName + " : si vous lisez ce message, les SMS fonctionnent."
	if len(os.Args) > 3 {
		message = strings.Join(os.Args[3:], " ")
	}
	sms, _, err := notify.NewSenders(cfg)
	if err != nil {
		return err
	}
	text := notify.PrepareSMS(message, cfg.SMS.ASCII, cfg.SMS.MaxSegments)
	segments, unicode := notify.SMSSegments(text)
	err = sms.SendSMS(ctx, os.Args[2], message)
	status, detail := notify.DeliveryStatus(sms, err)
	slog.Info("test SMS", "pilote", cfg.SMS.Driver, "statut", status, "detail", detail,
		"texte", text, "sms_factures", segments, "unicode", unicode)
	return err
}
