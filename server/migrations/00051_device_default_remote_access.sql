-- +goose Up
-- Device enrollment is a single trust decision. Backfill the complete Agent
-- permission profile and enable remote access for active credentials that were
-- created before registration became atomic with remote_access_grants.
UPDATE remote_device_access_keys
SET scopes = '["remote.connect","remote.peer.ai.config","remote.peer.ai.chat","remote.peer.ai.tools","remote.peer.events","remote.peer.file.receive","remote.peer.file.send","remote.peer.query","remote.peer.task.control","remote.peer.terminal","remote.peer.terminal.interactive"]'::jsonb,
    updated_at = now();

UPDATE remote_device_credentials
SET scopes = '["remote.connect","remote.peer.ai.config","remote.peer.ai.chat","remote.peer.ai.tools","remote.peer.events","remote.peer.file.receive","remote.peer.file.send","remote.peer.query","remote.peer.task.control","remote.peer.terminal","remote.peer.terminal.interactive"]'::jsonb,
    updated_at = now()
WHERE status = 'active';

INSERT INTO remote_access_grants
    (device_id, user_id, scopes, status, grant_version, enabled_at, revoked_at, created_at, updated_at)
SELECT credential.device_id, credential.user_id, '[]'::jsonb, 'enabled',
       credential.grant_version, now(), NULL, now(), now()
FROM remote_device_credentials credential
LEFT JOIN remote_access_grants access_grant ON access_grant.device_id = credential.device_id
WHERE credential.status = 'active' AND access_grant.device_id IS NULL;

-- +goose Down
-- Deliberately irreversible: restoring partial permissions or disabling an
-- automatically enabled device would require guessing prior user state.
