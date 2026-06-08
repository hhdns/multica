CREATE TABLE persona_llm_calls (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id      UUID        REFERENCES agent(id) ON DELETE CASCADE,
    workspace_id  UUID        REFERENCES workspace(id) ON DELETE CASCADE,
    call_type     TEXT        NOT NULL,
    backend       TEXT        NOT NULL,
    model         TEXT        NOT NULL,
    input_tokens  INT         NOT NULL DEFAULT 0,
    output_tokens INT         NOT NULL DEFAULT 0,
    latency_ms    INT         NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX persona_llm_calls_workspace_id_created_at ON persona_llm_calls (workspace_id, created_at DESC);
CREATE INDEX persona_llm_calls_agent_id_created_at ON persona_llm_calls (agent_id, created_at DESC);
