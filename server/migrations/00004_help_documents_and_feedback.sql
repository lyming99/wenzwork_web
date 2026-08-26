-- +goose Up
CREATE TABLE help_documents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug varchar(120) NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]+(?:-[a-z0-9]+)*$'),
    title varchar(160) NOT NULL CHECK (char_length(btrim(title)) > 0),
    description varchar(500) NOT NULL DEFAULT '',
    category varchar(80) NOT NULL CHECK (char_length(btrim(category)) > 0),
    sort_order integer NOT NULL DEFAULT 0 CHECK (sort_order BETWEEN -100000 AND 100000),
    content_markdown text NOT NULL CHECK (char_length(content_markdown) BETWEEN 1 AND 100000),
    status varchar(20) NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'archived')),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    published_version bigint CHECK (published_version IS NULL OR published_version > 0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    updated_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    published_by uuid REFERENCES users(id) ON DELETE SET NULL,
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (published_version IS NULL AND published_at IS NULL)
        OR (published_version IS NOT NULL AND published_at IS NOT NULL)
    )
);

CREATE INDEX help_documents_admin_order_idx
    ON help_documents (status, sort_order, updated_at DESC, id);

-- Each publication is an immutable, pre-rendered snapshot. Public requests read
-- only the version referenced by help_documents.published_version, so draft
-- edits never alter the live article until an explicit publish operation.
CREATE TABLE help_document_publications (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id uuid NOT NULL REFERENCES help_documents(id) ON DELETE CASCADE,
    version bigint NOT NULL CHECK (version > 0),
    slug varchar(120) NOT NULL CHECK (slug ~ '^[a-z0-9]+(?:-[a-z0-9]+)*$'),
    title varchar(160) NOT NULL,
    description varchar(500) NOT NULL DEFAULT '',
    category varchar(80) NOT NULL,
    sort_order integer NOT NULL,
    content_html text NOT NULL,
    search_text text NOT NULL,
    published_by uuid REFERENCES users(id) ON DELETE SET NULL,
    published_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, version)
);

CREATE INDEX help_document_publications_slug_idx
    ON help_document_publications (slug, version DESC);

CREATE TABLE feedback_entries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    category varchar(30) NOT NULL CHECK (category IN ('suggestion', 'bug', 'question', 'other')),
    subject varchar(160) NOT NULL CHECK (char_length(btrim(subject)) BETWEEN 1 AND 160),
    content text NOT NULL CHECK (char_length(btrim(content)) BETWEEN 1 AND 10000),
    contact_email varchar(320),
    status varchar(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'resolved', 'closed')),
    admin_reply text NOT NULL DEFAULT '' CHECK (char_length(admin_reply) <= 5000),
    internal_note text NOT NULL DEFAULT '' CHECK (char_length(internal_note) <= 5000),
    handled_by uuid REFERENCES users(id) ON DELETE SET NULL,
    resolved_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (contact_email IS NULL OR char_length(contact_email) BETWEEN 3 AND 320),
    CHECK (
        (status IN ('resolved', 'closed') AND resolved_at IS NOT NULL)
        OR (status IN ('pending', 'processing') AND resolved_at IS NULL)
    )
);

CREATE INDEX feedback_entries_user_created_idx
    ON feedback_entries (user_id, created_at DESC, id DESC);
CREATE INDEX feedback_entries_admin_status_idx
    ON feedback_entries (status, updated_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS feedback_entries;
DROP TABLE IF EXISTS help_document_publications;
DROP TABLE IF EXISTS help_documents;
