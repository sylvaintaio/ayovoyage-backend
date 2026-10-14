#!/usr/bin/env bash
# Déploiement depuis votre ordinateur, dans backend/ :
#
#   VPS=ubuntu@1.2.3.4 ./deploy/deploy.sh --setup   # première fois : prépare le serveur (setup-server.sh)
#   VPS=ubuntu@1.2.3.4 ./deploy/deploy.sh           # ensuite : nouvelle version de l'API
#
# Variables : VPS (obligatoire), ARCH (amd64 par défaut ; arm64 si `uname -m` affiche aarch64 sur
# le serveur), PORT (8081 par défaut), DOMAIN et CERTBOT_EMAIL (transmis à setup-server.sh).
set -euo pipefail
: "${VPS:?Indiquez le serveur, ex. VPS=ubuntu@1.2.3.4}"
ARCH=${ARCH:-amd64}
PORT=${PORT:-8081}
BACKEND=$(cd "$(dirname "$0")/.." && pwd)
VERSION=$(git -C "$BACKEND" rev-parse --short HEAD 2>/dev/null || date +%Y%m%d%H%M)

if [ "${1:-}" = "--setup" ]; then
  echo "→ Envoi du kit de déploiement"
  ssh "$VPS" 'rm -rf ~/busvoyage-deploy && mkdir -p ~/busvoyage-deploy'
  scp -q -r "$BACKEND/deploy/." "$VPS:busvoyage-deploy/"
  echo "→ Préparation du serveur (mot de passe sudo demandé)"
  ssh -t "$VPS" "cd ~/busvoyage-deploy && sudo DOMAIN='${DOMAIN:-}' PORT='$PORT' CERTBOT_EMAIL='${CERTBOT_EMAIL:-}' ./setup-server.sh; rm -rf ~/busvoyage-deploy/out"
  # Le fichier de valeurs locales a été appliqué : inutile de le garder sur cet ordinateur.
  rm -f "$BACKEND/deploy/out/api.env.overrides" "$BACKEND/deploy/out/firebase-service-account.json"
  echo "Serveur prêt. Lancer maintenant : VPS=$VPS ./deploy/deploy.sh"
  exit 0
fi

echo "→ Tests"
(cd "$BACKEND" && go vet ./... && go test ./... >/dev/null)

echo "→ Compilation linux/$ARCH ($VERSION)"
(cd "$BACKEND" && GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/api-linux ./cmd/api)

echo "→ Envoi"
scp -q "$BACKEND/bin/api-linux" "$VPS:/tmp/busvoyage-api"

echo "→ Installation et redémarrage"
ssh -t "$VPS" "
  set -e
  export LC_ALL=C.UTF-8
  sudo install -o root -g root -m 755 /tmp/busvoyage-api /opt/busvoyage/bin/api.new
  [ -f /opt/busvoyage/bin/api ] && sudo cp /opt/busvoyage/bin/api /opt/busvoyage/bin/api.previous || true
  sudo mv /opt/busvoyage/bin/api.new /opt/busvoyage/bin/api
  rm -f /tmp/busvoyage-api
  sudo systemctl restart busvoyage-api
  for i in \$(seq 1 30); do
    if curl -fsS http://127.0.0.1:$PORT/readyz >/dev/null 2>&1; then echo 'API prête.'; exit 0; fi
    sleep 1
  done
  echo 'L’API ne répond pas : sudo journalctl -u busvoyage-api -n 50'; exit 1
"
echo "✓ Déployé ($VERSION). Retour arrière : sudo mv /opt/busvoyage/bin/api.previous /opt/busvoyage/bin/api && sudo systemctl restart busvoyage-api"
