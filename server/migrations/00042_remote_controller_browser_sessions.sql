-- +goose Up
-- Browser controller identities are registered through the cookie-session
-- routes. Their session identifier therefore belongs to sessions, whereas
-- device credentials continue to be registered through app_sessions.
ALTER TABLE remote_controller_identities
    DROP CONSTRAINT IF EXISTS remote_controller_identities_registered_session_id_fkey;

ALTER TABLE remote_controller_identities
    ALTER COLUMN registered_session_id DROP NOT NULL;

-- There are no valid browser registrations under the old app_sessions
-- relation. Retain any historical identity, but clear an unmappable audit
-- reference before attaching the correct foreign key.
UPDATE remote_controller_identities AS controller
SET registered_session_id = NULL
WHERE NOT EXISTS (
    SELECT 1 FROM sessions AS browser_session
    WHERE browser_session.id = controller.registered_session_id
);

ALTER TABLE remote_controller_identities
    ADD CONSTRAINT remote_controller_identities_registered_session_id_fkey
    FOREIGN KEY (registered_session_id) REFERENCES sessions(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE remote_controller_identities
    DROP CONSTRAINT IF EXISTS remote_controller_identities_registered_session_id_fkey;

UPDATE remote_controller_identities AS controller
SET registered_session_id = NULL
WHERE registered_session_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM app_sessions AS app_session
      WHERE app_session.id = controller.registered_session_id
  );

ALTER TABLE remote_controller_identities
    ADD CONSTRAINT remote_controller_identities_registered_session_id_fkey
    FOREIGN KEY (registered_session_id) REFERENCES app_sessions(id) ON DELETE SET NULL;
