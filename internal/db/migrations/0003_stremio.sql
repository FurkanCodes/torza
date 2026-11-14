-- Stremio add-on credentials: a secret in the add-on's URL, separate from
-- API keys, so a leaked add-on URL can't use the API and can be revoked alone.
ALTER TABLE credentials DROP CONSTRAINT credentials_kind_check;
ALTER TABLE credentials ADD CONSTRAINT credentials_kind_check CHECK (kind IN ('api', 'stremio'));
ALTER TABLE credentials ADD COLUMN label TEXT NOT NULL DEFAULT '';
