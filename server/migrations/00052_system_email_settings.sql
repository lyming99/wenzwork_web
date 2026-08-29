-- +goose Up
CREATE TABLE system_email_settings (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    override_enabled boolean NOT NULL DEFAULT false,
    smtp_host varchar(255) NOT NULL DEFAULT '',
    smtp_port integer NOT NULL DEFAULT 0 CHECK (smtp_port BETWEEN 0 AND 65535),
    smtp_user varchar(320) NOT NULL DEFAULT '',
    smtp_password_ciphertext bytea,
    mail_from varchar(500) NOT NULL DEFAULT '',
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT system_email_override_shape CHECK (
        override_enabled = false
        OR (smtp_host <> '' AND smtp_port > 0 AND mail_from <> '')
    )
);

INSERT INTO system_email_settings (singleton) VALUES (true);

-- +goose Down
DROP TABLE IF EXISTS system_email_settings;
