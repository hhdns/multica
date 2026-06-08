-- bge-m3 produces 1024-dimensional embeddings; the column was originally
-- created for OpenAI's 1536-dim models. All existing embeddings are NULL
-- (no data to migrate), so we drop the index, alter the column, and
-- recreate the index with the correct dimension.
DROP INDEX IF EXISTS agent_memory_embedding_hnsw_idx;
ALTER TABLE agent_memory ALTER COLUMN embedding TYPE vector(1024);
CREATE INDEX agent_memory_embedding_hnsw_idx ON agent_memory
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
