#!/usr/bin/env python3
"""Prépare deploy/out/api.env.overrides à partir de backend/.env (sur votre ordinateur).

Reprend uniquement ce qui est propre à votre compte : nom de la marque, contacts, SMTP et SMS.
Les secrets de production (base, jetons, clé des billets) ne sont PAS repris : setup-server.sh
en génère de nouveaux sur le serveur.

    python3 deploy/make-overrides.py

Le fichier est ensuite envoyé par `deploy.sh --setup`, appliqué, puis supprimé des deux côtés.
Aucune valeur n'est affichée.
"""
import os
import re
import sys

BACKEND = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SOURCE = os.path.join(BACKEND, '.env')
TARGET = os.path.join(BACKEND, 'deploy', 'out', 'api.env.overrides')

KEYS = re.compile(
    r'^(BRAND_NAME|SUPPORT_[A-Z]+|OTP_CHANNELS|EMAIL_DRIVER|MAIL_HOST|MAIL_PORT|MAIL_USERNAME|'
    r'MAIL_PASSWORD|MAIL_FROM_ADDRESS|MAIL_FROM_NAME|EMAIL_FROM|SMS_[A-Z_]+|'
    r'PAYMENT_PROVIDERS|FEDAPAY_ENV|FEDAPAY_SECRET_KEY|FEDAPAY_WEBHOOK_SECRET)$'
)
SPECIAL = re.compile(r'[\s"&{};$#|<>`\\()]')


def parse_value(raw):
    """Lit une valeur comme le shell le ferait pour un .env simple."""
    raw = raw.strip()
    if raw[:1] in ("'", '"'):
        end = raw.find(raw[0], 1)
        return raw[1:end] if end > 0 else raw[1:]
    return re.sub(r'\s+#.*$', '', raw)


def quote(value):
    if not SPECIAL.search(value):
        return value
    if "'" in value:
        sys.exit("Valeur contenant une apostrophe : à reporter à la main dans api.env")
    return "'" + value + "'"


def main():
    if len(sys.argv) > 1:
        sys.exit('make-overrides.py ne prend aucun argument. Lancer deploy.sh dans une commande séparée :\n'
                 '  python3 deploy/make-overrides.py\n'
                 '  VPS=utilisateur@serveur ./deploy/deploy.sh --setup')
    if not os.path.exists(SOURCE):
        sys.exit(f'{SOURCE} introuvable')
    values = {}
    with open(SOURCE, encoding='utf-8') as f:
        for line in f:
            m = re.match(r'^([A-Z0-9_]+)=(.*)$', line.rstrip('\n'))
            if m and KEYS.match(m.group(1)):
                values[m.group(1)] = parse_value(m.group(2))
    # Clé Firebase : le fichier est envoyé à part (deploy/out, jamais versionné) et installé
    # sur le serveur dans /etc/busvoyage, lisible par l'API seulement.
    firebase = None
    with open(SOURCE, encoding='utf-8') as f:
        for line in f:
            m = re.match(r'^FIREBASE_CREDENTIALS_FILE=(.*)$', line.rstrip('\n'))
            if m:
                firebase = os.path.expanduser(parse_value(m.group(1)))
    if firebase:
        if not os.path.isfile(firebase):
            sys.exit(f'FIREBASE_CREDENTIALS_FILE introuvable : {firebase}')
        os.makedirs(os.path.join(BACKEND, 'deploy', 'out'), exist_ok=True)
        dest = os.path.join(BACKEND, 'deploy', 'out', 'firebase-service-account.json')
        fd = os.open(dest, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, 'wb') as out, open(firebase, 'rb') as src:
            out.write(src.read())
        values['FIREBASE_CREDENTIALS_FILE'] = '/etc/busvoyage/firebase-service-account.json'

    # En production, le pilote « log » des e-mails n'a pas de sens.
    if values.get('EMAIL_DRIVER') == 'log':
        values.pop('EMAIL_DRIVER')

    os.makedirs(os.path.dirname(TARGET), exist_ok=True)
    fd = os.open(TARGET, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w', encoding='utf-8') as out:
        out.write('# Généré par make-overrides.py depuis backend/.env — ne pas versionner.\n')
        for key, value in values.items():
            out.write(f'{key}={quote(value)}\n')

    empty = [k for k, v in values.items() if v == '']
    print(f'{TARGET} : {len(values)} variables ({", ".join(values)})')
    if empty:
        print('Vides (à compléter ensuite sur le serveur si besoin) : ' + ', '.join(empty))


if __name__ == '__main__':
    main()
