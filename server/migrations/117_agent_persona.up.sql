CREATE TABLE agent_persona (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id UUID NOT NULL UNIQUE REFERENCES agent(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,

    -- Trait scores 0–100; 50 = neutral baseline
    trait_thoroughness  INTEGER NOT NULL DEFAULT 50 CHECK (trait_thoroughness  BETWEEN 0 AND 100),
    trait_verbosity     INTEGER NOT NULL DEFAULT 50 CHECK (trait_verbosity     BETWEEN 0 AND 100),
    trait_risk_appetite INTEGER NOT NULL DEFAULT 50 CHECK (trait_risk_appetite BETWEEN 0 AND 100),
    trait_curiosity     INTEGER NOT NULL DEFAULT 50 CHECK (trait_curiosity     BETWEEN 0 AND 100),
    trait_confidence    INTEGER NOT NULL DEFAULT 50 CHECK (trait_confidence    BETWEEN 0 AND 100),

    strengths   TEXT[] NOT NULL DEFAULT '{}',
    blind_spots TEXT[] NOT NULL DEFAULT '{}',

    -- Short-term state; updated after task runs
    mood            TEXT        NOT NULL DEFAULT 'calm',
    mood_updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- 0 = fully deterministic, 100 = highly spontaneous
    variance_level INTEGER NOT NULL DEFAULT 30 CHECK (variance_level BETWEEN 0 AND 100),

    -- LLM-synthesised (or hand-written) self-description
    identity TEXT,

    -- Total signals folded into this persona so far
    signal_count        INTEGER     NOT NULL DEFAULT 0,
    last_synthesized_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE agent_interaction_signal (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id     UUID NOT NULL REFERENCES agent(id)   ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,

    -- praise | criticism | task_success | task_failure | rework_requested
    type    TEXT NOT NULL,
    content TEXT NOT NULL,
    weight  REAL NOT NULL DEFAULT 0.5,

    -- comment | system
    source_type    TEXT NOT NULL DEFAULT 'comment',
    source_id      UUID,
    source_user_id UUID REFERENCES "user"(id) ON DELETE SET NULL,

    processed  BOOLEAN     NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_agent_interaction_signal_agent
    ON agent_interaction_signal(agent_id, created_at DESC);

CREATE INDEX idx_agent_interaction_signal_unprocessed
    ON agent_interaction_signal(agent_id)
    WHERE NOT processed;
