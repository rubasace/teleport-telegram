package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupRequiresExplicitApprover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "0", "-1", "@username", "12,34", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TELEGRAM_APPROVER_ID", value)
			if err := run(context.Background(), path); err == nil || !strings.Contains(err.Error(), "TELEGRAM_APPROVER_ID") {
				t.Fatalf("startup did not reject approver %q: %v", value, err)
			}
		})
	}
}
