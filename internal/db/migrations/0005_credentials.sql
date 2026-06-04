-- 0005_credentials - the credential store for the bundled credential proxy
-- (brief §8). Tokens are stored ENCRYPTED at rest (AES-256-GCM); the ciphertext
-- column holds iv:tag:ciphertext (base64). The encryption key is NOT stored here
-- - it comes from GOCLAW_SECRET_ENCRYPTION_KEY in the environment, so a stolen
-- data dir / DB dump does not include the key.
--
-- The proxy matches an outbound request to a credential by target host and
-- injects the token (api.anthropic.com -> x-api-key, otherwise Authorization:
-- Bearer). id is a host-assigned UUID; name is a human label for `goclaw auth
-- list`.
--
-- Pure DDL: connection pragmas are applied per-connection in db.go.

CREATE TABLE IF NOT EXISTS credentials (
    id               TEXT PRIMARY KEY,           -- UUID, assigned on add
    name             TEXT NOT NULL,              -- human label, e.g. "anthropic"
    target_url       TEXT NOT NULL,              -- e.g. https://api.anthropic.com
    target_host      TEXT NOT NULL,              -- parsed host, e.g. api.anthropic.com (match key)
    token_ciphertext TEXT NOT NULL,              -- iv:tag:ciphertext, base64 (AES-256-GCM)
    created_at       TEXT NOT NULL DEFAULT (datetime('now'))
);

-- One credential per target host (the proxy resolves by host).
CREATE UNIQUE INDEX IF NOT EXISTS idx_credentials_host ON credentials(target_host);
