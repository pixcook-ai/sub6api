-- Encrypted login credentials for automatic OpenAI re-authorization (phase two of batch login).
-- Password and TOTP secret are encrypted by the application (AES-256-GCM) before they reach this table.
CREATE TABLE IF NOT EXISTS openai_login_credentials (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    login_email TEXT NOT NULL,
    target TEXT NOT NULL CHECK (target IN ('bps', 'codex')),
    password_ciphertext TEXT NOT NULL,
    totp_ciphertext TEXT NOT NULL,
    fail_count INTEGER NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ,
    last_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
