-- Base de l'API sur un PostgreSQL existant (partagé avec d'autres projets).
-- Lancé par setup-server.sh ; à la main :
--
--   sudo -u postgres psql \
--     -v owner_password='MOT_DE_PASSE_OWNER' \
--     -v app_password='MOT_DE_PASSE_APP' \
--     -f 01-database.sql
--
-- Relançable : les rôles existants reçoivent le nouveau mot de passe, la base existante est conservée.
--
-- Deux rôles distincts :
--   busvoyage_owner : propriétaire du schéma, utilisé uniquement pour les migrations ;
--   busvoyage_app   : rôle de l'application, sans droit de modifier le schéma et soumis
--                     à l'isolation entre compagnies (Row-Level Security).
-- Les autres bases du serveur ne sont pas touchées.

\set ON_ERROR_STOP on

SELECT format('CREATE ROLE busvoyage_owner LOGIN PASSWORD %L', :'owner_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'busvoyage_owner') \gexec
SELECT format('ALTER ROLE busvoyage_owner LOGIN PASSWORD %L', :'owner_password') \gexec

SELECT format('CREATE ROLE busvoyage_app LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS', :'app_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'busvoyage_app') \gexec
SELECT format('ALTER ROLE busvoyage_app LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS', :'app_password') \gexec

SELECT 'CREATE DATABASE busvoyage OWNER busvoyage_owner ENCODING ''UTF8'' TEMPLATE template0'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'busvoyage') \gexec

REVOKE ALL ON DATABASE busvoyage FROM PUBLIC;
GRANT CONNECT ON DATABASE busvoyage TO busvoyage_owner, busvoyage_app;

\connect busvoyage

ALTER SCHEMA public OWNER TO busvoyage_owner;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO busvoyage_app;

-- Tout objet créé par le propriétaire (migrations, y compris la file de tâches)
-- est automatiquement accessible en lecture/écriture au rôle applicatif.
ALTER DEFAULT PRIVILEGES FOR ROLE busvoyage_owner IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO busvoyage_app;
ALTER DEFAULT PRIVILEGES FOR ROLE busvoyage_owner IN SCHEMA public
    GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO busvoyage_app;
ALTER DEFAULT PRIVILEGES FOR ROLE busvoyage_owner IN SCHEMA public
    GRANT EXECUTE ON FUNCTIONS TO busvoyage_app;
