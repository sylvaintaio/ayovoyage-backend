#!/usr/bin/env bash
# RECETTE UNIQUEMENT — remplace les données du serveur par la base locale (Docker) :
# comptes de démonstration, lignes, départs, billets, terminaux…
#
#   VPS=deploy@1.2.3.4 ./deploy/sync-local-data.sh      (depuis backend/, mot de passe sudo demandé)
#
# Pour que les données copiées fonctionnent sur le serveur :
#   - la clé des billets de backend/.env est reprise (les QR codes déjà émis restent valides) ;
#     avant l'ouverture réelle, en changer : voir docs/operations.md (api ticket-key) ;
#   - la double authentification du personnel est réinitialisée (ses secrets sont chiffrés
#     avec une clé propre à chaque machine) : chacun la réactive à sa prochaine connexion ;
#   - toutes les sessions sont fermées.
# Refusé si le serveur est en APP_ENV=production. Une sauvegarde est faite avant le remplacement.
set -euo pipefail
: "${VPS:?Indiquez le serveur, ex. VPS=deploy@1.2.3.4}"
BACKEND=$(cd "$(dirname "$0")/.." && pwd)
CONTAINER=${CONTAINER:-busvoyage-postgres-1}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "→ Export de la base locale ($CONTAINER)"
docker exec "$CONTAINER" pg_dump -U postgres -d busvoyage --no-owner --no-privileges > "$TMP/busvoyage.sql"
echo "  $(grep -c '^COPY ' "$TMP/busvoyage.sql") tables, $(du -h "$TMP/busvoyage.sql" | cut -f1)"

KEY=$(sed -n 's/^TICKET_SIGNING_KEY=//p' "$BACKEND/.env" | tr -d "'\" ")
KID=$(sed -n 's/^TICKET_SIGNING_KEY_ID=//p' "$BACKEND/.env" | tr -d "'\" ")
[ -n "$KEY" ] || { echo "TICKET_SIGNING_KEY absente de backend/.env" >&2; exit 1; }
(umask 077 && printf 'TICKET_SIGNING_KEY=%s\nTICKET_SIGNING_KEY_ID=%s\n' "$KEY" "${KID:-k1}" > "$TMP/ticket.env")

cat > "$TMP/after-import.sql" <<'SQL'
SELECT set_config('app.scope', 'system', false);
UPDATE staff_users SET totp_secret_enc = NULL, totp_enabled_at = NULL, totp_last_step = 0, mfa_required_from = now();
DELETE FROM staff_recovery_codes;
DELETE FROM refresh_tokens;
SQL

echo "→ Envoi"
ssh "$VPS" 'mkdir -p ~/busvoyage-sync && chmod 700 ~/busvoyage-sync'
scp -q "$TMP/busvoyage.sql" "$TMP/ticket.env" "$TMP/after-import.sql" "$BACKEND/deploy/import-data.sh" "$VPS:busvoyage-sync/"

echo "→ Remplacement des données sur le serveur (mot de passe sudo demandé)"
ssh -t "$VPS" '
  set -e
  export LC_ALL=C.UTF-8
  cd ~/busvoyage-sync
  trap "rm -rf ~/busvoyage-sync" EXIT
  ENV=/etc/busvoyage/api.env
  if sudo grep -q "^APP_ENV=production" "$ENV"; then echo "Refusé : serveur en production."; exit 1; fi

  # Clé des billets de la base locale (remplace les lignes existantes).
  sudo awk "NR==FNR { split(\$0, kv, \"=\"); keys[kv[1]] = \$0; next }
            { split(\$0, kv, \"=\"); if (kv[1] in keys) { print keys[kv[1]]; delete keys[kv[1]] } else print }
            END { for (k in keys) print keys[k] }" ticket.env "$ENV" | sudo tee "$ENV.tmp" >/dev/null
  sudo install -o root -g busvoyage -m 640 "$ENV.tmp" "$ENV" && sudo rm -f "$ENV.tmp"

  chmod +x import-data.sh
  sudo FORCE=1 ./import-data.sh "$PWD/busvoyage.sql"

  OWNER_URL=$(sudo sed -n "s/^MIGRATION_DATABASE_URL=//p" "$ENV" | tr -d "\x27\"")
  psql "$OWNER_URL" -q -v ON_ERROR_STOP=1 -f after-import.sql >/dev/null
  echo "Double authentification réinitialisée, sessions fermées."
'
echo "✓ Données locales copiées sur le serveur."
