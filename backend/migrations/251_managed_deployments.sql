-- Selection changes only new sessions; durable sessions never expire into a different binding.
CREATE TABLE managed_deployments (
 deployment_id TEXT PRIMARY KEY,
 group_id BIGINT NOT NULL UNIQUE REFERENCES groups(id) ON DELETE CASCADE,
 route_revision BIGINT NOT NULL CHECK(route_revision > 0),
 selected_provider TEXT NOT NULL CHECK(selected_provider IN ('bps_excel','codex')),
 bps_binding JSONB NOT NULL,
 codex_binding JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE managed_deployment_sessions (
 deployment_id TEXT NOT NULL REFERENCES managed_deployments(deployment_id) ON DELETE CASCADE,
 user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
 session_key TEXT NOT NULL,
 route_revision BIGINT NOT NULL,
 binding JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY(deployment_id,user_id,api_key_id,session_key)
);
CREATE TABLE managed_deployment_resources (
 deployment_id TEXT NOT NULL,
 user_id BIGINT NOT NULL,
 api_key_id BIGINT NOT NULL,
 resource_kind TEXT NOT NULL,
 resource_hash TEXT NOT NULL,
 session_key TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY(deployment_id,user_id,api_key_id,resource_kind,resource_hash),
 FOREIGN KEY(deployment_id,user_id,api_key_id,session_key) REFERENCES managed_deployment_sessions(deployment_id,user_id,api_key_id,session_key) ON DELETE CASCADE
);
