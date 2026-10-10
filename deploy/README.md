# Déployer l'API sur le VPS

Cible : serveur Ubuntu avec **Nginx déjà installé** (partagé avec d'autres sites). PostgreSQL est installé par le script s'il manque. Le site web est sur **Vercel**.

```
Navigateur ──► ayovoyage.vercel.app (Vercel : site Angular)
                    │  /api/*  relayé par Vercel
                    ▼
               api.ayovoyage.sylvaintaio.com (VPS : Nginx ──► API Go sur 127.0.0.1:8081 ──► PostgreSQL)
                    ▲
Applications mobiles, webhooks FedaPay (appel direct)
```

L'API est **un seul fichier exécutable** : ni Go, ni Node, ni Docker sur le serveur. Les migrations et les tâches planifiées (rappels, expirations, purges) sont intégrées. Elle consomme 50 à 100 Mo de mémoire et ne touche pas aux autres sites.

| Fichier | Où | Rôle |
|---|---|---|
| `deploy.sh` | ordinateur | `--setup` : prépare le serveur ; sans option : compile et installe une nouvelle version |
| `make-overrides.py` | ordinateur | Reprend de `backend/.env` le SMTP, le SMS et le nom de la marque (sans rien afficher) |
| `setup-server.sh` | serveur | Vérifications, PostgreSQL, base et rôles, secrets, service, sauvegarde, Nginx, HTTPS |
| `01-database.sql` | serveur | Base `busvoyage` et rôles `busvoyage_owner` (migrations) / `busvoyage_app` (application) |
| `api.env.example` | serveur | Modèle de `/etc/busvoyage/api.env` |
| `busvoyage-api.service` | serveur | Service systemd (redémarrage automatique, durcissement) |
| `nginx-api.conf` | serveur | Site Nginx de l'API |
| `backup.sh`, `busvoyage-backup.cron` | serveur | Sauvegarde de la base chaque nuit (14 jours) |
| `import-data.sh` | serveur | Facultatif : charger un export de la base locale |

---

## Avant de commencer

- [ ] Le DNS `api.ayovoyage.sylvaintaio.com` pointe vers le serveur (enregistrement A).
- [ ] Vous avez un accès SSH avec `sudo` : `ssh utilisateur@IP`.
- [ ] Architecture du serveur : `ssh utilisateur@IP uname -m` → `x86_64` (par défaut) ou `aarch64` (ajouter `ARCH=arm64` aux commandes `deploy.sh`).
- [ ] Sur votre ordinateur : Go installé, et `backend/.env` contient les bons réglages SMTP et SMS.

## 1. Préparer le serveur (une seule fois)

Depuis votre ordinateur, dans `backend/` :

```bash
python3 deploy/make-overrides.py
VPS=utilisateur@IP CERTBOT_EMAIL=votre@email ./deploy/deploy.sh --setup
```

Le script envoie le kit, demande le mot de passe `sudo` et lance `setup-server.sh`, qui :

1. vérifie Nginx, installe PostgreSQL s'il manque, vérifie que le port **8081** est libre (sinon relancer avec `PORT=8082`) ;
2. crée l'utilisateur système `busvoyage`, le service `busvoyage-api` et la sauvegarde nocturne ;
3. crée la base `busvoyage` et ses deux rôles avec des **mots de passe générés** ;
4. écrit `/etc/busvoyage/api.env` avec des **secrets générés** (`JWT_SECRET`, `OTP_SECRET`, `TICKET_SIGNING_KEY`, `MFA_ENCRYPTION_KEY`), puis y ajoute le SMTP, le SMS et le nom de la marque préparés par `make-overrides.py`. Ce fichier est ensuite supprimé du serveur et de l'ordinateur ;
5. installe le site Nginx et obtient le certificat HTTPS (certbot).

Le script peut être relancé sans risque : la base et `api.env` existants sont conservés.

> **Sauvegardez `api.env` hors du serveur** (gestionnaire de mots de passe) : `ssh -t utilisateur@IP sudo cat /etc/busvoyage/api.env`. Perdre `TICKET_SIGNING_KEY` invalide tous les billets. Changer `MFA_ENCRYPTION_KEY` oblige le personnel à refaire sa double authentification.

## 2. Installer l'API

```bash
VPS=utilisateur@IP ./deploy/deploy.sh
```

Le script lance les tests, compile pour Linux, envoie le binaire, redémarre le service et attend la réponse de l'API. Au premier démarrage, l'API crée toutes les tables.

```bash
curl https://api.ayovoyage.sylvaintaio.com/readyz    # 200 = prête
```

## 3. Compléter la configuration

```bash
ssh -t utilisateur@IP sudo nano /etc/busvoyage/api.env
ssh -t utilisateur@IP sudo systemctl restart busvoyage-api
```

| À vérifier | Valeur |
|---|---|
| `SUPPORT_PHONE`, `SUPPORT_EMAIL`, `SUPPORT_WHATSAPP` | Contacts affichés dans les e-mails |
| `APP_ENV` | `staging` pour la recette (paiement simulé). `production` à l'ouverture, avec `PAYMENT_PROVIDERS=fedapay`, `FEDAPAY_SECRET_KEY`, `FEDAPAY_WEBHOOK_SECRET` |
| `SMS_SENDER_ID` | Vide tant que `AYOVOYAGE` n'est pas approuvé par Africa's Talking |
| `OTP_CHANNELS` | `email` ; `email,sms` une fois les SMS validés |

Règles d'écriture du fichier : **aucun commentaire en fin de ligne**, et **apostrophes** autour d'une valeur qui contient une espace, `"`, `&`, `{`, `;`, `$` ou `#`.

## 4. Données de départ (au choix)

**Base vide** (production réelle) : créer la compagnie et son premier administrateur, sur le serveur :
```bash
sudo -u busvoyage bash -c 'set -a; . /etc/busvoyage/api.env; set +a; /opt/busvoyage/bin/api create-company --name "Nom de la compagnie" --admin-email admin@exemple.com'
```
Le mot de passe est généré et affiché **une seule fois** : notez-le, connectez-vous sur `/admin/connexion`, activez la double authentification et changez le mot de passe. Options : `--admin-name`, `--admin-phone`, `--legal-name`, `--phone`, `--email`, `--slug`. Utilisez des guillemets droits `"…"`, pas `« … »`. L'administrateur crée ensuite les villes, agences, bus, lignes et départs dans le back-office.

**Données de démonstration** générées (recette uniquement, refusé en `production`) :
```bash
ssh -t utilisateur@IP "sudo -u busvoyage bash -c 'set -a; . /etc/busvoyage/api.env; set +a; /opt/busvoyage/bin/api seed'"
```

**Reprendre la base de votre ordinateur** (comptes, réservations de test…) :
```bash
docker exec busvoyage-postgres-1 pg_dump -U postgres -d busvoyage --no-owner --no-privileges > busvoyage.sql
scp busvoyage.sql deploy/import-data.sh utilisateur@IP:~
ssh -t utilisateur@IP "sudo FORCE=1 ~/import-data.sh ~/busvoyage.sql && rm ~/busvoyage.sql"
```
Une sauvegarde est faite avant le remplacement. Les billets de l'export ne sont valides que si la clé des billets du serveur est celle de l'ordinateur d'origine.

## 5. Vérifications

- [ ] `https://ayovoyage.vercel.app` affiche les départs ; une page profonde (`/compte/reservations`) se recharge sans erreur 404.
- [ ] Connexion client : le code arrive par e-mail.
- [ ] Back-office : connexion et activation de la double authentification.
- [ ] Une réservation complète (paiement simulé en recette).
- [ ] `ssh -t utilisateur@IP sudo journalctl -u busvoyage-api -n 50` : pas d'erreur, et les journaux montrent l'IP réelle des visiteurs (pas celle de Vercel).
- [ ] Sauvegarde : `ssh -t utilisateur@IP "sudo /opt/busvoyage/backup.sh && sudo ls -lh /var/backups/busvoyage"`.

---

## Au quotidien

| Besoin | Commande (sur le serveur sauf mention) |
|---|---|
| Nouvelle version | `VPS=utilisateur@IP ./deploy/deploy.sh` (depuis `backend/` sur l'ordinateur) |
| Revenir à la version précédente | `sudo mv /opt/busvoyage/bin/api.previous /opt/busvoyage/bin/api && sudo systemctl restart busvoyage-api` |
| État du service | `sudo systemctl status busvoyage-api` |
| Journaux en direct | `sudo journalctl -u busvoyage-api -f` |
| Modifier la configuration | `sudo nano /etc/busvoyage/api.env && sudo systemctl restart busvoyage-api` |
| Tester l'envoi d'un SMS | `sudo -u busvoyage bash -c 'set -a; . /etc/busvoyage/api.env; set +a; /opt/busvoyage/bin/api sms-test +229XXXXXXXXXX'` |
| Sauvegarde manuelle | `sudo /opt/busvoyage/backup.sh` |
| Restaurer une sauvegarde | `sudo systemctl stop busvoyage-api && sudo -u postgres pg_restore --clean --if-exists -d busvoyage /var/backups/busvoyage/FICHIER.dump && sudo systemctl start busvoyage-api` |
| Copier les sauvegardes hors du serveur | `scp utilisateur@IP:/var/backups/busvoyage/*.dump ./sauvegardes/` (depuis l'ordinateur, régulièrement) |

## Sécurité

- L'API n'écoute que sur `127.0.0.1` : seul Nginx est exposé.
- Le service tourne sous un utilisateur sans droits, avec un système de fichiers en lecture seule et 512 Mo de mémoire au plus.
- `/etc/busvoyage/api.env` n'est lisible que par root et le service.
- Le rôle `busvoyage_app` ne peut ni modifier le schéma ni contourner l'isolation entre compagnies.
- Nginx transmet l'IP fournie par Vercel (`X-Real-IP`). Un appel direct à l'API pourrait falsifier cet en-tête. Cela ne touche que la limitation de débit par IP : les codes de connexion et les mots de passe ont leurs propres limites par compte.
