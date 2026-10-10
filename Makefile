# Commandes usuelles du backend. Les variables sont lues dans .env.
SHELL := /bin/bash
ENV := set -a && . ./.env && set +a

.PHONY: help db sqlc build run run-local migrate seed openapi test vet fmt check docker

help: ## Affiche cette aide
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

db: ## Démarre PostgreSQL (Docker)
	docker compose -f ../docker-compose.yml up -d postgres

sqlc: ## Régénère le code Go depuis db/queries/*.sql
	sqlc generate

build: ## Compile le binaire dans bin/api
	CGO_ENABLED=0 go build -trimpath -o bin/api ./cmd/api

run: ## Lance l'API (http://localhost:8080/docs)
	$(ENV) && go run ./cmd/api serve

run-local: ## Lance l'API sans envoi réel : e-mails et SMS écrits dans les journaux (tests)
	$(ENV) && EMAIL_DRIVER=log SMS_DRIVER=log go run ./cmd/api serve 2>&1 | tee /tmp/busvoyage-api.log

migrate: ## Applique les migrations
	$(ENV) && go run ./cmd/api migrate

seed: ## Crée les données de démonstration
	$(ENV) && go run ./cmd/api seed

openapi: ## Exporte api/openapi.yaml (3.1) et api/openapi-3.0.yaml
	go run ./cmd/api openapi

test: ## Tests unitaires
	go test ./...

vet: ## Analyse statique
	go vet ./...

fmt: ## Formate le code
	gofmt -w cmd internal db

check: fmt vet test ## Vérifications avant commit
	@git diff --exit-code -- internal/store api || (echo "Code généré ou OpenAPI non à jour : make sqlc openapi" && exit 1)

docker: ## Construit l'image Docker
	docker build -t busvoyage-api .
