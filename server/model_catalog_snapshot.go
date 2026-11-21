package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Keep the complete upstream JSON in the repository. All builds, including
// backend-only builds, carry the catalog without runtime files or network I/O.
//
//go:embed modelcatalog/models.dev.v2.json
var bundledModelCatalogJSON []byte

//go:embed modelcatalog/metadata.json
var bundledModelCatalogMetadata []byte

var bundledModelCatalog = sync.OnceValues(func() (*modelCatalog, error) {
	var metadata struct {
		Source    string    `json:"source"`
		FetchedAt time.Time `json:"fetched_at"`
	}
	if err := json.Unmarshal(bundledModelCatalogMetadata, &metadata); err != nil {
		return nil, fmt.Errorf("read bundled model catalog metadata: %w", err)
	}
	providers, err := parseModelCatalog(bundledModelCatalogJSON)
	if err != nil {
		return nil, fmt.Errorf("read bundled model catalog: %w", err)
	}
	return &modelCatalog{Source: metadata.Source, UpdatedAt: metadata.FetchedAt, Origin: "bundled", Providers: providers}, nil
})
