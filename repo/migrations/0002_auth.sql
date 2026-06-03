-- Cairn step-3 schema: users, cookie sessions, resumable upload sessions, and
-- reconciliation of libraries.owner (text) into a proper owner_id FK.

CREATE TABLE users (
    id            uuid        PRIMARY KEY,
    username      text        NOT NULL UNIQUE,
    password_hash text        NOT NULL,                  -- argon2id PHC string
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Reconcile libraries.owner -> owner_id. Dev data is throwaway, so we seed one
-- LOCKED user per distinct existing owner string and remap. '!' is not a valid
-- argon2id PHC string, so argon2id verification always fails for these seeded
-- users until an admin sets a real password via `cairn-server create-user`.
ALTER TABLE libraries ADD COLUMN owner_id uuid REFERENCES users(id);

INSERT INTO users (id, username, password_hash)
SELECT gen_random_uuid(), owner, '!'
FROM (SELECT DISTINCT owner FROM libraries) d
ON CONFLICT (username) DO NOTHING;

UPDATE libraries l SET owner_id = u.id FROM users u WHERE u.username = l.owner;

ALTER TABLE libraries DROP COLUMN owner;
ALTER TABLE libraries ALTER COLUMN owner_id SET NOT NULL;
CREATE INDEX idx_libraries_owner ON libraries (owner_id);

-- Cookie sessions. Only a HASH of the token is stored, never the raw token.
CREATE TABLE sessions (
    token_hash bytea       PRIMARY KEY,                  -- sha256(raw token)
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    user_agent text,                                     -- reserved for audit
    ip         text                                      -- reserved for audit
);
CREATE INDEX idx_sessions_user    ON sessions (user_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);

-- Resumable upload sessions. One temp file per session (named by the server).
CREATE TABLE upload_sessions (
    id             uuid        PRIMARY KEY,
    library_id     uuid        NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    user_id        uuid        NOT NULL REFERENCES users(id)     ON DELETE CASCADE,
    target_path    text        NOT NULL,                 -- destination directory (virtual)
    filename       text        NOT NULL,
    declared_size  bigint,                               -- nullable
    received_bytes bigint      NOT NULL DEFAULT 0 CHECK (received_bytes >= 0),
    temp_path      text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL
);
CREATE INDEX idx_upload_sessions_expires ON upload_sessions (expires_at);
