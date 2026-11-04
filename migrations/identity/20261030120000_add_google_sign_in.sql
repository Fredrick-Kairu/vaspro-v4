-- +goose Up

-- Task 1.16, Sign in with Google (D-22, decided 30 Oct; data model §3).

-- External sign-in accounts linked to a user. Copied exactly from data
-- model §3. Global like users: no tenant_id, no RLS.
CREATE TABLE identity.user_identities (            -- external sign-in accounts linked to a user
  id              uuid PRIMARY KEY,
  user_id         uuid NOT NULL REFERENCES identity.users (id),
  provider        text NOT NULL CHECK (provider IN ('google')),   -- more later, e.g. microsoft, apple
  subject         text NOT NULL,                     -- the provider's stable user ID (OIDC sub), never the email
  email_at_link   citext NOT NULL,                   -- the email the provider reported when linked
  hosted_domain   text,                              -- Google Workspace domain (hd claim), if any
  linked_at       timestamptz NOT NULL DEFAULT now(),
  last_used_at    timestamptz,
  UNIQUE (provider, subject),
  UNIQUE (user_id, provider)                         -- one Google account per user
);

-- How a session was signed in (data model §3). Existing rows are
-- backfilled to 'password' through the temporary default, then the
-- default is dropped so every new session must name its method. This
-- rewrites the method of pre-existing sessions (some were passkey
-- sessions); approved for 1.16 because nothing is deployed yet, so
-- only development and test data exist.
ALTER TABLE identity.sessions
  ADD COLUMN auth_method text NOT NULL DEFAULT 'password'
    CHECK (auth_method IN ('password', 'passkey', 'google', 'sso'));
ALTER TABLE identity.sessions ALTER COLUMN auth_method DROP DEFAULT;

-- Google sign-in must refuse an email whose domain has enabled SSO,
-- before any tenant is known. sso_connections has FORCE ROW LEVEL
-- SECURITY, so a plain lookup silently sees no rows (1.7's finding).
-- Same answer as 1.8's identity_token_exchange: a NOLOGIN role with a
-- SELECT-only policy, reached only through db.Pool.WithRoleTx, and
-- granted WITH INHERIT FALSE so it never applies outside that
-- transaction.
CREATE ROLE identity_sso_domain_lookup NOLOGIN;
GRANT USAGE ON SCHEMA identity TO identity_sso_domain_lookup;
GRANT SELECT ON identity.sso_connections TO identity_sso_domain_lookup;
CREATE POLICY sso_domain_lookup_read ON identity.sso_connections
  FOR SELECT TO identity_sso_domain_lookup
  USING (true);
GRANT identity_sso_domain_lookup TO CURRENT_USER WITH INHERIT FALSE;

-- +goose Down

REVOKE identity_sso_domain_lookup FROM CURRENT_USER;
DROP POLICY sso_domain_lookup_read ON identity.sso_connections;
REVOKE SELECT ON identity.sso_connections FROM identity_sso_domain_lookup;
REVOKE USAGE ON SCHEMA identity FROM identity_sso_domain_lookup;
DROP ROLE identity_sso_domain_lookup;

ALTER TABLE identity.sessions DROP COLUMN auth_method;

DROP TABLE identity.user_identities;
