#!/usr/bin/env bash
# FACULTATIF — charge sur le serveur un export SQL de la base locale (données de démonstration
# ou de recette), à la place d'une base vide. À lancer SUR le serveur :
#
#   sudo ./import-data.sh ~/busvoyage-AAAAMMJJ-HHMM.sql            # base encore vide
#   sudo FORCE=1 ./import-data.sh ~/busvoyage-AAAAMMJJ-HHMM.sql    # REMPLACE toutes les données
#
# Export à produire sur votre ordinateur (format SQL, lisible par toute version de PostgreSQL) :
#   docker exec busvoyage-postgres-1 pg_dump -U postgres -d busvoyage --no-owner --no-privileges > busvoyage.sql
#
# Les données sont chargées avec le rôle busvoyage_owner, pour que l'application garde ses droits.
# Les billets déjà émis dans l'export ne seront valides que si TICKET_SIGNING_KEY est la même
# que celle de l'ordinateur d'origine.
set -euo pipefail
FILE=${1:?Indiquez le fichier .sql à importer}
ENV_FILE=/etc/busvoyage/api.env
[ "$(id -u)" = 0 ] || { echo "Lancer avec sudo" >&2; exit 1; }
[ -f "$FILE" ] || { echo "Fichier introuvable : $FILE" >&2; exit 1; }

OWNER_URL=$(sed -n 's/^MIGRATION_DATABASE_URL=//p' "$ENV_FILE" | tr -d "'\"")
[ -n "$OWNER_URL" ] || { echo "MIGRATION_DATABASE_URL absent de $ENV_FILE" >&2; exit 1; }

if psql "$OWNER_URL" -Atc "select to_regclass('public.goose_db_version') is not null" | grep -q t; then
  if [ "${FORCE:-}" != 1 ]; then
    echo "La base contient déjà des données. Relancer avec FORCE=1 pour TOUT remplacer." >&2
    exit 1
  fi
  echo "→ Sauvegarde de sécurité avant remplacement"
  /opt/busvoyage/backup.sh
fi

echo "→ Arrêt de l'API"
systemctl stop busvoyage-api || true

if [ "${FORCE:-}" = 1 ]; then
  echo "→ Vidage du schéma"
  PGOPTIONS="-c client_min_messages=warning" psql "$OWNER_URL" -q -v ON_ERROR_STOP=1 <<'SQL'
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO busvoyage_app;
-- Les droits par défaut sont liés au schéma : à recréer avec lui (voir 01-database.sql).
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO busvoyage_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO busvoyage_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO busvoyage_app;
SQL
fi

echo "→ Import"
# transaction_timeout n'existe qu'à partir de PostgreSQL 17 : ligne retirée pour les versions antérieures.
grep -v '^SET transaction_timeout' "$FILE" | psql "$OWNER_URL" -q -v ON_ERROR_STOP=1 >/dev/null

echo "→ Redémarrage de l'API"
systemctl start busvoyage-api
for i in $(seq 1 30); do
  PORT=$(sed -n 's/^HTTP_ADDR=.*://p' "$ENV_FILE")
  if curl -fsS "http://127.0.0.1:${PORT:-8081}/readyz" >/dev/null 2>&1; then echo "✓ Import terminé, API prête."; exit 0; fi
  sleep 1
done
echo "L’API ne répond pas : sudo journalctl -u busvoyage-api -n 50" >&2
exit 1
