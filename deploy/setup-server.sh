#!/usr/bin/env bash
# Préparation du serveur (Ubuntu, Nginx déjà en place), à lancer SUR le serveur :
#
#   sudo ./setup-server.sh
#   sudo DOMAIN=api.exemple.com PORT=8082 CERTBOT_EMAIL=moi@exemple.com ./setup-server.sh
#
# Relançable sans risque. Il :
#   1. vérifie Nginx et PostgreSQL (installe PostgreSQL s'il manque) ;
#   2. crée l'utilisateur système, les dossiers, le service systemd et la sauvegarde nocturne ;
#   3. crée la base et ses deux rôles, avec des mots de passe générés (première fois seulement) ;
#   4. écrit /etc/busvoyage/api.env avec des secrets générés (première fois seulement),
#      puis y applique out/api.env.overrides s'il est présent (SMTP, SMS, contacts) ;
#   5. installe le site Nginx et obtient le certificat HTTPS (certbot).
# Le binaire de l'API est envoyé ensuite depuis votre ordinateur par deploy.sh.
set -euo pipefail
cd "$(dirname "$0")"
# Le Mac transmet LC_CTYPE=UTF-8 par SSH, inconnu d'Ubuntu : évite les avertissements de perl/psql.
export LC_ALL=C.UTF-8
unset LC_CTYPE

DOMAIN=${DOMAIN:-api.ayovoyage.sylvaintaio.com}
PORT=${PORT:-8081}
CERTBOT_EMAIL=${CERTBOT_EMAIL:-}
ENV_FILE=/etc/busvoyage/api.env
OVERRIDES=out/api.env.overrides

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[31mErreur : %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || fail "lancer avec sudo"

# ---------------------------------------------------------------- 1. Vérifications
step "Vérifications"
command -v nginx >/dev/null || fail "Nginx introuvable"
command -v openssl >/dev/null || fail "openssl introuvable"
command -v curl >/dev/null || fail "curl introuvable"
if ! command -v psql >/dev/null; then
  echo "PostgreSQL absent : installation du paquet Ubuntu…"
  apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql
fi
systemctl is-active --quiet postgresql || systemctl start postgresql
PGVER=$(sudo -u postgres psql -Atc 'show server_version_num')
[ "$PGVER" -ge 150000 ] || fail "PostgreSQL 15 ou plus récent requis (version $PGVER)"
echo "Nginx $(nginx -v 2>&1 | sed 's#.*/##'), PostgreSQL $(sudo -u postgres psql -Atc 'show server_version')"

# Port libre, ou déjà occupé par cette API (relance du script).
if ss -ltnH "sport = :$PORT" | grep -q . && ! systemctl is-active --quiet busvoyage-api \
   && ! curl -fsS "http://127.0.0.1:$PORT/readyz" >/dev/null 2>&1; then
  fail "le port $PORT est déjà utilisé par un autre programme : relancer avec PORT=8082 (par exemple)"
fi

# ---------------------------------------------------------------- 2. Système
step "Utilisateur, dossiers, service, sauvegarde"
id busvoyage >/dev/null 2>&1 || useradd --system --home /opt/busvoyage --shell /usr/sbin/nologin busvoyage
install -d -o root -g root -m 755 /opt/busvoyage /opt/busvoyage/bin
install -d -o root -g busvoyage -m 750 /etc/busvoyage
install -o root -g root -m 644 busvoyage-api.service /etc/systemd/system/busvoyage-api.service
install -o root -g root -m 755 backup.sh /opt/busvoyage/backup.sh
install -o root -g root -m 644 busvoyage-backup.cron /etc/cron.d/busvoyage-backup
systemctl daemon-reload
systemctl enable busvoyage-api >/dev/null 2>&1
echo "Service busvoyage-api installé (démarrera après le premier envoi du binaire)."

# ---------------------------------------------------------------- 3 et 4. Base et configuration
if [ -f "$ENV_FILE" ]; then
  step "Configuration existante conservée ($ENV_FILE)"
  echo "Base et secrets inchangés."
else
  step "Base de données"
  # Hexadécimal : aucun caractère à encoder dans les URL de connexion.
  OWNER_PW=$(openssl rand -hex 24)
  APP_PW=$(openssl rand -hex 24)
  sudo -u postgres psql -q -v owner_password="$OWNER_PW" -v app_password="$APP_PW" -f 01-database.sql
  echo "Base busvoyage et rôles busvoyage_owner / busvoyage_app prêts."

  step "Configuration $ENV_FILE"
  b64() { openssl rand -base64 32; }
  sed -e "s|__DOMAIN__|$DOMAIN|g" \
      -e "s|__PORT__|$PORT|g" \
      -e "s|__DB_APP_PASSWORD__|$APP_PW|" \
      -e "s|__DB_OWNER_PASSWORD__|$OWNER_PW|" \
      -e "s|__JWT_SECRET__|$(b64)|" \
      -e "s|__OTP_SECRET__|$(b64)|" \
      -e "s|__TICKET_SIGNING_KEY__|$(b64)|" \
      -e "s|__MFA_ENCRYPTION_KEY__|$(b64)|" \
      api.env.example > "$ENV_FILE.tmp"
  install -o root -g busvoyage -m 640 "$ENV_FILE.tmp" "$ENV_FILE"
  rm -f "$ENV_FILE.tmp"
  echo "Secrets générés. À sauvegarder hors du serveur : sudo cat $ENV_FILE"
fi

if [ -f "$OVERRIDES" ]; then
  step "Application de $OVERRIDES"
  # Les clés présentes dans le fichier remplacent celles d'api.env (les autres sont conservées).
  awk 'NR == FNR { if ($0 ~ /^[A-Z0-9_]+=/) { split($0, kv, "="); keys[kv[1]] = 1; extra[++n] = $0 } next }
       { if ($0 ~ /^[A-Z0-9_]+=/) { split($0, kv, "="); if (kv[1] in keys) next } print }
       END { print ""; print "# --- Valeurs reprises de api.env.overrides"; for (i = 1; i <= n; i++) print extra[i] }' \
      "$OVERRIDES" "$ENV_FILE" > "$ENV_FILE.tmp"
  install -o root -g busvoyage -m 640 "$ENV_FILE.tmp" "$ENV_FILE"
  rm -f "$ENV_FILE.tmp" "$OVERRIDES"
  echo "$(grep -c '^[A-Z]' "$ENV_FILE") variables configurées ; fichier d'origine supprimé."
fi

grep -q '__[A-Z_]*__' "$ENV_FILE" && fail "valeurs __…__ non remplacées dans $ENV_FILE"

# ---------------------------------------------------------------- 5. Nginx et HTTPS
step "Nginx ($DOMAIN)"
SITE=/etc/nginx/sites-available/$DOMAIN
if [ -f "$SITE" ]; then
  echo "Site existant conservé ($SITE)."
else
  sed -e "s|__DOMAIN__|$DOMAIN|g" -e "s|__PORT__|$PORT|g" nginx-api.conf > "$SITE"
  ln -sf "$SITE" "/etc/nginx/sites-enabled/$DOMAIN"
fi
nginx -t
systemctl reload nginx

if grep -q 'ssl_certificate' "$SITE"; then
  echo "HTTPS déjà configuré."
elif command -v certbot >/dev/null; then
  step "Certificat HTTPS"
  if certbot --nginx -d "$DOMAIN" --non-interactive --redirect ${CERTBOT_EMAIL:+--agree-tos -m "$CERTBOT_EMAIL"}; then
    echo "HTTPS actif."
  else
    echo "Certbot a échoué. Vérifier que $DOMAIN pointe vers ce serveur, puis :"
    echo "  sudo certbot --nginx -d $DOMAIN"
  fi
else
  echo "Certbot absent : sudo apt install certbot python3-certbot-nginx && sudo certbot --nginx -d $DOMAIN"
fi

step "Terminé"
cat <<EOF
Prochaines étapes :
  1. Depuis votre ordinateur, dans backend/ :  VPS=utilisateur@serveur ./deploy/deploy.sh
  2. Vérifier :  curl https://$DOMAIN/readyz
  3. Compléter si besoin $ENV_FILE (contacts, SMS, FedaPay), puis : sudo systemctl restart busvoyage-api
EOF
