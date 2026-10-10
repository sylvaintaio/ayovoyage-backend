// Package database gère le pool PostgreSQL et les transactions "scopées"
// (positionnement du contexte multi-tenant utilisé par le Row-Level Security).
package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"busvoyage/internal/store"
)

// Scope détermine les lignes visibles par les politiques RLS.
type Scope string

const (
	ScopeStaff    Scope = "staff"    // limité à la compagnie de l'employé
	ScopeCustomer Scope = "customer" // client : filtrage par customer_id dans les requêtes
	ScopePublic   Scope = "public"   // données publiques (recherche, agences)
	ScopeSystem   Scope = "system"   // tâches de fond, authentification
)

// Tenant décrit le contexte d'exécution d'une transaction.
type Tenant struct {
	Scope     Scope
	CompanyID uuid.UUID // obligatoire pour ScopeStaff
}

func System() Tenant   { return Tenant{Scope: ScopeSystem} }
func Public() Tenant   { return Tenant{Scope: ScopePublic} }
func Customer() Tenant { return Tenant{Scope: ScopeCustomer} }
func Staff(companyID uuid.UUID) Tenant {
	return Tenant{Scope: ScopeStaff, CompanyID: companyID}
}

type DB struct {
	Pool *pgxpool.Pool
}

func Connect(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	// Pool volontairement modeste : l'API reste légère en ressources.
	if cfg.MaxConns > 20 {
		cfg.MaxConns = 20
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

// Tx exécute fn dans une transaction dont le contexte RLS est positionné.
// Toute erreur (ou panique) annule la transaction.
func (d *DB) Tx(ctx context.Context, t Tenant, fn func(tx pgx.Tx, q *store.Queries) error) error {
	if t.Scope == ScopeStaff && t.CompanyID == uuid.Nil {
		return errors.New("database: scope staff sans compagnie")
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // sans effet après Commit

	companyID := ""
	if t.CompanyID != uuid.Nil {
		companyID = t.CompanyID.String()
	}
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.scope', $1, true), set_config('app.company_id', $2, true)",
		string(t.Scope), companyID); err != nil {
		return fmt.Errorf("positionnement du contexte RLS: %w", err)
	}
	if err := fn(tx, store.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// UniqueViolation renvoie le nom de la contrainte violée, ou "" sinon.
func UniqueViolation(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
