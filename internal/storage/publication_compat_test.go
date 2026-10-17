package storage

import "context"

// PutSourceSetThenMetadata verifies all selected source objects, reconciles an
// exact predecessor, writes the frozen next metadata and verifies its exact body.
// Missing ref-only sources and mismatching immutable objects are never replaced.
// At most 65 references and 128 MiB per object are checked sequentially; total
// referenced bytes may be larger. A completed uncertain Put replays idempotently.
func PutSourceSetThenMetadata(ctx context.Context, store ObjectStore, sources []SourcePublication, key string, metadata []byte, prior MetadataPredecessor, retry RetryPolicy) error {
	return PutResolvedSourceSetThenMetadata(ctx, store, sources, key, metadata, prior, retry, nil, nil)
}

// PutResolvedSourceSetThenMetadata resolves one payload at a time and shares
// the same predecessor/readback protocol as inline publication.
func PutResolvedSourceSetThenMetadata(ctx context.Context, store ObjectStore, sources []SourcePublication, key string, metadata []byte, prior MetadataPredecessor, retry RetryPolicy, resolve SourceResolver, verify SourceVerifier) error {
	_, err := PutResolvedSourceSetThenMetadataReadback(ctx, store, sources, key, metadata, prior, retry, resolve, verify)
	return err
}
