-- +goose Up

-- Task 2.7: data model §4 says business profiles aren't unique per
-- registration number, "duplicates are flagged for review instead of
-- blocked". Counting other accounts' profiles with the same number
-- reads across accounts, which business_profiles' tenant policy
-- forbids. A narrow NOLOGIN role, reached only through
-- db.Pool.WithRoleTx and granted WITH INHERIT FALSE so it never applies
-- outside that transaction (1.8's lesson), may read business_profiles
-- and nothing else; app.SubmitKYB only counts. An addition to data
-- model §4, like 2.3a's and 2.6's roles. The service's own login role
-- needs the same grant where it isn't the role running migrations.
CREATE ROLE accounts_registration_lookup NOLOGIN;
GRANT USAGE ON SCHEMA accounts TO accounts_registration_lookup;
GRANT SELECT ON accounts.business_profiles TO accounts_registration_lookup;
CREATE POLICY registration_lookup ON accounts.business_profiles
  FOR SELECT TO accounts_registration_lookup
  USING (true);
GRANT accounts_registration_lookup TO CURRENT_USER WITH INHERIT FALSE;

-- +goose Down
DROP POLICY registration_lookup ON accounts.business_profiles;
REVOKE SELECT ON accounts.business_profiles FROM accounts_registration_lookup;
REVOKE USAGE ON SCHEMA accounts FROM accounts_registration_lookup;
DROP ROLE accounts_registration_lookup;
