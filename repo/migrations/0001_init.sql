-- Cairn step-2 schema: Git-like file/tree/commit objects in Postgres on top of
-- the step-1 content-addressed block store.
--
-- Content hashes are stored as CHAR(64) lowercase hex. `namespace` columns are
-- reserved for a future per-library encrypted mode and are always 'global' now.

CREATE TABLE libraries (
    id                 UUID        PRIMARY KEY,
    name               TEXT        NOT NULL,
    owner              TEXT        NOT NULL,
    head_commit_hash   CHAR(64),                          -- NULL = empty library (no commits yet)
    encrypted          BOOLEAN     NOT NULL DEFAULT FALSE,
    enc_version        INTEGER,                           -- reserved, unused
    salt               BYTEA,                             -- reserved, unused
    encrypted_file_key BYTEA,                             -- reserved, unused
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Authoritative refcount. The block store (Badger/FS) is now a dumb byte store;
-- its in-store Incr/Decr are deprecated. refcount = number of commit rows that
-- reference the block, counted once per distinct block per commit. It MAY
-- temporarily include commits that are orphaned (unreachable from head, e.g. a
-- crash between tx1 and tx2); GC reconciles. This is the safe direction: a
-- reachable block is never under-counted.
CREATE TABLE blocks (
    block_hash CHAR(64) PRIMARY KEY,
    namespace  TEXT     NOT NULL DEFAULT 'global',        -- reserved
    size       BIGINT   NOT NULL,
    refcount   BIGINT   NOT NULL DEFAULT 0 CHECK (refcount >= 0)
);

-- Immutable, content-addressed file & tree objects. Insert-only, deduped
-- globally by obj_hash. content holds the EXACT canonical bytes such that
-- obj_hash = sha256(content) (verified by fsck). No library_id: under global
-- dedup it would only record the first writer; per-library linkage lives in the
-- commit chain.
CREATE TABLE fs_objects (
    obj_hash  CHAR(64) PRIMARY KEY,
    namespace TEXT     NOT NULL DEFAULT 'global',          -- reserved
    type      TEXT     NOT NULL CHECK (type IN ('tree', 'file')),
    content   BYTEA    NOT NULL
);

CREATE TABLE commits (
    commit_hash    CHAR(64)    PRIMARY KEY,
    library_id     UUID        NOT NULL REFERENCES libraries(id),
    parent_hash    CHAR(64)    REFERENCES commits(commit_hash),            -- NULL = root commit
    root_tree_hash CHAR(64)    NOT NULL REFERENCES fs_objects(obj_hash),   -- rule A: tree must exist first
    ctime          TIMESTAMPTZ NOT NULL DEFAULT now(),
    description    TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX idx_commits_library ON commits (library_id);

-- rule A: head can only ever point at an existing commit. Added after `commits`
-- exists because libraries <-> commits form a circular reference.
ALTER TABLE libraries
    ADD CONSTRAINT fk_head_commit
    FOREIGN KEY (head_commit_hash) REFERENCES commits (commit_hash);

-- Derived cache, rebuilt from a commit's tree. `path` is the PARENT directory of
-- the entry (root listing uses path = '/'); `name` is the entry within it.
CREATE TABLE path_index (
    library_id    UUID        NOT NULL REFERENCES libraries(id),
    path          TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    is_dir        BOOLEAN     NOT NULL,
    size          BIGINT      NOT NULL DEFAULT 0,
    mtime         TIMESTAMPTZ,
    file_obj_hash CHAR(64),                                -- NULL for directories
    PRIMARY KEY (library_id, path, name)
);

-- Directory listing and exact-name lookup are served by the PK prefix
-- (library_id, path[, name]). This index adds case-insensitive prefix/pattern
-- search on the entry name across a library.
CREATE INDEX idx_path_index_name_pattern
    ON path_index (library_id, lower(name) text_pattern_ops);
