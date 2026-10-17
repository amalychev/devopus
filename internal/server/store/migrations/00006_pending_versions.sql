-- +goose Up
-- A version with risky changes waiting for the device's user (risk R21).
ALTER TABLE device_environments ADD COLUMN pending_version integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE device_environments DROP COLUMN pending_version;
