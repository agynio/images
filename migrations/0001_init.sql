CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE images (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id   UUID NOT NULL,
    name              TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    type              TEXT NOT NULL,
    repository        TEXT NOT NULL,
    username          TEXT NOT NULL DEFAULT '',
    secret_id         UUID,
    visibility        TEXT NOT NULL,
    tag_filter        TEXT NOT NULL DEFAULT '',
    stale_since       TIMESTAMPTZ,
    last_discovery_at TIMESTAMPTZ,
    -- When discovery may next claim this image. Claiming sets it forward, so a
    -- crashed pass is retried rather than blocking the image forever.
    discovery_due_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (organization_id, name)
);

CREATE INDEX images_visibility_idx ON images (visibility);
CREATE INDEX images_discovery_due_idx ON images (discovery_due_at);

CREATE TABLE image_versions (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    image_id      UUID NOT NULL REFERENCES images (id) ON DELETE CASCADE,
    tag           TEXT NOT NULL,
    pushed_at     TIMESTAMPTZ,
    description   TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'present',
    discovered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (image_id, tag)
);

CREATE INDEX image_versions_image_idx ON image_versions (image_id, state);
