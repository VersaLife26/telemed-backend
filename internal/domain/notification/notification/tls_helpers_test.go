package notification

import "os"

func writeCAFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
