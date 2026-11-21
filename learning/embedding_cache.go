package learning

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"trpc.group/trpc-go/trpc-agent-go/knowledge/embedder"
)

// cachedEmbedder preserves native embeddings across restarts. Parsing, chunking,
// retrieval and scoring remain owned by native Knowledge and VectorStore.
type cachedEmbedder struct {
	embedder.Embedder
	db        *sql.DB
	namespace string
}

func (c *cachedEmbedder) key(text string) string {
	sum := sha256.Sum256([]byte(c.namespace + "\x00" + text))
	return hex.EncodeToString(sum[:])
}
func (c *cachedEmbedder) GetEmbedding(ctx context.Context, text string) ([]float64, error) {
	v, _, err := c.GetEmbeddingWithUsage(ctx, text)
	return v, err
}
func (c *cachedEmbedder) GetEmbeddingWithUsage(ctx context.Context, text string) ([]float64, map[string]any, error) {
	key := c.key(text)
	var raw string
	err := c.db.QueryRowContext(ctx, `SELECT embedding FROM embeddings WHERE id=?`, key).Scan(&raw)
	if err == nil {
		var v []float64
		if json.Unmarshal([]byte(raw), &v) == nil && len(v) == c.GetDimensions() {
			return v, map[string]any{"cached": true}, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	v, usage, err := c.Embedder.GetEmbeddingWithUsage(ctx, text)
	if err != nil {
		return nil, nil, err
	}
	if len(v) != c.GetDimensions() {
		return nil, nil, fmt.Errorf("embedding dimension: got %d, expected %d", len(v), c.GetDimensions())
	}
	for _, n := range v {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, nil, errors.New("invalid embedding value")
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO embeddings(id,embedding) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET embedding=excluded.embedding`, key, string(data)); err != nil {
		return nil, nil, err
	}
	return v, usage, nil
}
