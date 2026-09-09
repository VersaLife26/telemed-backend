package observability

import "telemed/internal/platform/logger"

func safePathForTest(p string) string { return logger.SafePath(p) }
