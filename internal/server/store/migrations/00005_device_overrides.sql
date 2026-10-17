-- +goose Up

-- Personal overrides (devopus.yaml) a device applied, and the ones it refused: the
-- drift teamleads see.
ALTER TABLE device_environments ADD COLUMN overrides text[] NOT NULL DEFAULT '{}';
ALTER TABLE device_environments ADD COLUMN rejected_overrides text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE device_environments DROP COLUMN rejected_overrides;
ALTER TABLE device_environments DROP COLUMN overrides;
