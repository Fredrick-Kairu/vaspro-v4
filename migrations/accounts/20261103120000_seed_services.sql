-- +goose Up

-- Task 2.4: the service catalogue (OQ-06, the requirements' tier
-- table; Voice/IVR is out of scope). Each country's pack may override a
-- service's minimum tier or turn it off (service_country_rules, synced
-- from packs/<cc>/pack.yaml at startup). Own sender IDs, shortcodes and
-- dedicated USSD codes are applications on top of these (Phase 12), not
-- services of their own.
INSERT INTO accounts.services (code, name, default_min_tier) VALUES
  ('sms',         'SMS (shared sender ID)',     'starter'),
  ('surveys',     'Surveys',                    'starter'),
  ('ussd',        'USSD (shared channel)',      'starter'),
  ('whatsapp',    'WhatsApp',                   'business'),
  ('rewards',     'Rewards (airtime and data)', 'business'),
  ('payouts',     'Payouts (B2C/B2B)',          'financial'),
  ('collections', 'Collections',                'financial');

-- +goose Down
DELETE FROM accounts.service_country_rules;
DELETE FROM accounts.services
WHERE code IN ('sms', 'surveys', 'ussd', 'whatsapp', 'rewards', 'payouts', 'collections');
