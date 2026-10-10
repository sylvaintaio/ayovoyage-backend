package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/mail"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/config"
	"busvoyage/internal/network"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/phone"
	"busvoyage/internal/store"
)

// createCompany : crée une compagnie et son premier administrateur, depuis le serveur.
//
//	api create-company --name "Ayò Compagnie" --admin-email admin@exemple.com [--admin-name …] [--admin-phone …]
//
// Le mot de passe est généré et affiché une seule fois (ou lu dans ADMIN_PASSWORD).
// La double authentification est exigée à la connexion, comme pour tout administrateur.
func createCompany(ctx context.Context, cfg config.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("create-company", flag.ContinueOnError)
	fs.SetOutput(out)
	name := fs.String("name", "", "nom commercial de la compagnie (obligatoire)")
	slug := fs.String("slug", "", "identifiant dans les adresses (déduit du nom par défaut)")
	legalName := fs.String("legal-name", "", "raison sociale (factures)")
	companyPhone := fs.String("phone", "", "téléphone de la compagnie")
	companyEmail := fs.String("email", "", "e-mail de contact de la compagnie")
	adminEmail := fs.String("admin-email", "", "e-mail de connexion de l'administrateur (obligatoire)")
	adminName := fs.String("admin-name", "Administrateur", "nom complet de l'administrateur")
	adminPhone := fs.String("admin-phone", "", "téléphone de l'administrateur")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("argument inattendu %q (valeurs avec espaces entre guillemets droits \"…\")", fs.Arg(0))
	}

	*name = strings.TrimSpace(*name)
	if *name == "" || *adminEmail == "" {
		fs.Usage()
		return errors.New("--name et --admin-email sont obligatoires")
	}
	if *slug == "" {
		*slug = network.Slugify(*name)
	}
	if *slug == "" {
		return errors.New("impossible de déduire --slug du nom : le préciser")
	}
	addr, err := mail.ParseAddress(*adminEmail)
	if err != nil {
		return fmt.Errorf("--admin-email invalide : %w", err)
	}
	for _, p := range []*string{companyPhone, adminPhone} {
		if *p != "" {
			if *p, err = phone.Normalize(*p, cfg.DefaultCountryCode); err != nil {
				return fmt.Errorf("téléphone %q invalide : %w", *p, err)
			}
		}
	}

	password, generated := os.Getenv("ADMIN_PASSWORD"), false
	if password == "" {
		if password, err = randomPassword(20); err != nil {
			return err
		}
		generated = true
	} else if len(password) < 12 {
		return errors.New("ADMIN_PASSWORD : 12 caractères au moins")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Pool.Close()

	var company store.Company
	err = db.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		if _, err := q.GetStaffByEmail(ctx, addr.Address); err == nil {
			return fmt.Errorf("un compte du personnel utilise déjà %s", addr.Address)
		} else if !database.IsNotFound(err) {
			return err
		}
		company, err = q.CreateCompany(ctx, store.CreateCompanyParams{
			Name: *name, Slug: *slug, LegalName: optional(*legalName), Phone: optional(*companyPhone), Email: optional(*companyEmail),
		})
		if err != nil {
			if strings.Contains(err.Error(), "companies_slug_key") {
				return fmt.Errorf("l'identifiant %q est déjà pris : choisir un autre --slug", *slug)
			}
			return err
		}
		_, err = q.CreateStaff(ctx, store.CreateStaffParams{
			CompanyID: company.ID, Email: addr.Address, PasswordHash: hash,
			FullName: strings.TrimSpace(*adminName), Phone: *adminPhone, Role: auth.RoleCompanyAdmin,
		})
		return err
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\nCompagnie créée : %s (identifiant %s)\n", company.Name, company.Slug)
	fmt.Fprintf(out, "Administrateur   : %s\n", addr.Address)
	if generated {
		fmt.Fprintf(out, "Mot de passe     : %s\n", password)
		fmt.Fprintln(out, "                   (affiché une seule fois : le noter, puis le changer après la première connexion)")
	}
	fmt.Fprintf(out, "Connexion        : %s/admin/connexion (double authentification à activer)\n\n", strings.TrimRight(cfg.PublicWebURL, "/"))
	return nil
}

func optional(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

// randomPassword : lettres et chiffres sans caractères ambigus (0/O, 1/l/I).
func randomPassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[k.Int64()]
	}
	return string(b), nil
}
