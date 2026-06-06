-- 0006_credential_kind - credentials gain a KIND so the store can hold more than a
-- static token (docs/oauth-credentials.md). The encrypted payload (token_ciphertext)
-- stays opaque, but its PLAINTEXT shape now depends on kind:
--
--   static        - the blob is the token itself (today's behavior; the default).
--   oauth2-bearer - the blob is a JSON OAuth2 bundle (refresh token + client creds +
--                   token_url + scopes + a cached access token), refreshed host-side.
--   (future: atproto-dpop, etc. - the column is open-ended on purpose.)
--
-- Additive and safe: existing rows default to 'static' and behave exactly as before.
-- Migrations are tracked by version number, so this runs once.

ALTER TABLE credentials ADD COLUMN kind TEXT NOT NULL DEFAULT 'static';
