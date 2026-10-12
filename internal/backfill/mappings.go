package backfill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/config"
	"path/filepath"
	"strings"
)

// ParseProjectMappings decodes exact cwd mappings. The first equals sign is the separator.
func ParseProjectMappings(values []string) (map[string]string, error) {
	if len(values) > 128 {
		return nil, errors.New("--map-project permits at most 128 mappings")
	}
	mappings := map[string]string{}
	for _, value := range values {
		old, target, ok := strings.Cut(value, "=")
		if !ok || !filepath.IsAbs(old) || !filepath.IsAbs(target) || len(old) > 4096 || len(target) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("--map-project requires absolute OLD_CWD=CONFIGURED_ROOT paths")
		}
		old, target = filepath.Clean(old), filepath.Clean(target)
		if prior, ok := mappings[old]; ok && prior != target {
			return nil, errors.New("--map-project has conflicting mappings for one cwd")
		}
		mappings[old] = target
	}
	return mappings, nil
}

func validateProjectMappings(env Environment, cfg config.Config, mappings map[string]string) error {
	if len(mappings) > MappingCountLimit {
		return errors.New("too many project mappings")
	}
	included := map[string]bool{}
	for _, project := range cfg.Archive.Projects {
		if project.Included {
			included[filepath.Clean(project.Root)] = true
		}
	}
	observed := map[string]bool{}
	for old, target := range mappings {
		if !filepath.IsAbs(old) || !filepath.IsAbs(target) || len(old) > 4096 || len(target) > 4096 || strings.ContainsAny(old+target, "\x00\r\n") {
			return errors.New("invalid project mapping")
		}
		target = filepath.Clean(target)
		if observed[target] {
			continue
		}
		info, err := env.stat(target)
		if !included[target] || err != nil || !info.IsDir() {
			return errors.New("--map-project target must be an existing included configured root")
		}
		observed[target] = true
	}
	return nil
}

func mappingDigest(mappings map[string]string) string {
	if len(mappings) == 0 {
		return ""
	}
	b, _ := json.Marshal(mappings)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
