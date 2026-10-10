#!/usr/bin/env bash
# Sauvegarde quotidienne de la base de l’API (conservée 14 jours).
# Installé dans /opt/busvoyage/backup.sh, lancé par /etc/cron.d/busvoyage-backup.
set -euo pipefail
DIR=/var/backups/busvoyage
mkdir -p "$DIR"
STAMP=$(date +%Y-%m-%d_%H%M)
sudo -u postgres pg_dump --format=custom --compress=9 busvoyage > "$DIR/busvoyage_$STAMP.dump"
chmod 600 "$DIR/busvoyage_$STAMP.dump"
find "$DIR" -name 'busvoyage_*.dump' -mtime +14 -delete
