// Package nativecapture exposes only opaque verified native evidence.
package nativecapture

import "github.com/wangjohn/agent-archive/internal/agents/nativecapture/internal/sealed"

// Proof lends actually captured bytes and shares terminal Close across copies.
type Proof = sealed.Proof
