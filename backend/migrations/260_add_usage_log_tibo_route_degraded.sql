-- NULL: Tibo observation did not apply (including historical rows).
-- FALSE: a healthy/unknown route served the request; TRUE: degraded route.
-- Observation only: this field does not change account scheduling or billing.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS route_degraded BOOLEAN;
