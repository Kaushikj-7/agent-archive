package sourcefacts

import "github.com/wangjohn/agent-archive/internal/transcriptio"

// RootOpener preserves root-descriptor confinement when using verified snapshots.
// Its generic filesystem implementation lives at the transcript I/O boundary.
type RootOpener = transcriptio.RootOpener
