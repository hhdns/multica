DROP INDEX IF EXISTS agent_memory_embedding_hnsw_idx;
ALTER TABLE agent_memory ALTER COLUMN embedding TYPE vector(1536);
CREATE INDEX agent_memory_embedding_hnsw_idx ON agent_memory
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
