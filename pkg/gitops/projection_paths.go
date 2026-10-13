package gitops

import (
	"fmt"
	"path"
	"strings"
)

const serviceAccountTokenProjection = "serviceAccountToken"

// projectedTokenConflict checks the complete payload the kubelet assembles.
// A Secret or ConfigMap without items can supply any key, including token;
// reuse cannot establish disjointness without an explicit path list.
func projectedTokenConflict(sources []any, tokenIndex int, tokenPath string) string {
	for index, raw := range sources {
		source, _ := raw.(map[string]any)
		for kind, value := range source {
			if index == tokenIndex && kind == serviceAccountTokenProjection {
				continue
			}
			projection, _ := value.(map[string]any)
			paths, known := projectionPaths(kind, projection)
			if !known {
				return fmt.Sprintf("projected source %d (%s) has unbounded or unsupported paths beside %q", index, kind, tokenPath)
			}
			for _, other := range paths {
				left, right := path.Clean(tokenPath), path.Clean(other)
				if right == "." || left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/") {
					return fmt.Sprintf("projected source %d (%s) path %q collides with token path %q", index, kind, other, tokenPath)
				}
			}
		}
	}
	return ""
}

func projectionPaths(kind string, projection map[string]any) ([]string, bool) {
	switch kind {
	case serviceAccountTokenProjection, "clusterTrustBundle":
		file, ok := projection["path"].(string)
		return []string{file}, ok && file != ""
	case "secret", "configMap", "downwardAPI":
		items := sliceField(projection, "items")
		if len(items) == 0 {
			return nil, false
		}
		paths := make([]string, 0, len(items))
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			file, ok := item["path"].(string)
			if !ok || file == "" {
				return nil, false
			}
			paths = append(paths, file)
		}
		return paths, true
	default:
		return nil, false
	}
}
