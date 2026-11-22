package toolkit

import "context"

type Artifact struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Size      int    `json:"size"`
	SHA256    string `json:"sha256"`
	Complete  bool   `json:"complete"`
}
type Artifacts interface {
	Put(context.Context, []byte, string, bool) (Artifact, error)
	Read(context.Context, string) ([]byte, error)
}
type artifactKey struct{}

func WithArtifacts(ctx context.Context, store Artifacts) context.Context {
	return context.WithValue(ctx, artifactKey{}, store)
}
func ArtifactsFrom(ctx context.Context) Artifacts {
	v, _ := ctx.Value(artifactKey{}).(Artifacts)
	return v
}
