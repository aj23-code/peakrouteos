package cert

import (
	"fmt"
	"os/exec"
	"runtime"
)

// CommitToAlpineLBU invokes 'lbu commit -d' to commit volatile changes to persistent boot media.
// It gracefully skips execution if not running on Linux or if lbu is not installed.
func CommitToAlpineLBU() error {
	// Only execute on Linux systems
	if runtime.GOOS != "linux" {
		return nil
	}

	// Check if 'lbu' command exists on the host
	if _, err := exec.LookPath("lbu"); err != nil {
		// lbu is not available (e.g., standard Debian/Ubuntu or development workstation)
		return nil
	}

	// Run 'lbu commit -d'
	cmd := exec.Command("lbu", "commit", "-d")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("lbu commit failed: %w (output: %s)", err, string(output))
	}

	return nil
}
