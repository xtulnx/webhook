//go:build !windows

package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalUpdateAndRollback(t *testing.T) {
	const current = "0.2.0-dev.t20261123153000"
	const next = "0.2.0-dev.t20261123153001"
	dir := t.TempDir()
	target := filepath.Join(dir, "installed")
	source := filepath.Join(dir, "candidate")
	stateDir := filepath.Join(dir, "state")
	for path, version := range map[string]string{target: current, source: next} {
		script := fmt.Sprintf("#!/bin/sh\nprintf 'webhook version %%s\\n' '%s'\n", version)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	result, err := CheckLocal(ctx, source, target)
	if err != nil || !result.Available || result.CurrentVersion != current || result.LatestVersion != next {
		t.Fatalf("CheckLocal = %+v, %v", result, err)
	}
	if _, err := ApplyLocal(ctx, target, target, stateDir); err == nil {
		t.Fatal("ApplyLocal accepted identical source and target")
	}
	// An unusable rollback path must be rejected before replacing the target.
	blockedState := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(statePath(blockedState), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLocal(ctx, source, target, blockedState); err == nil {
		t.Fatal("ApplyLocal accepted a directory as the rollback state file")
	}
	if version, err := localVersion(ctx, target); err != nil || version != current {
		t.Fatalf("version after failed update = %q, %v", version, err)
	}

	state, err := ApplyLocal(ctx, source, target, stateDir)
	if err != nil || state.CurrentVersion != current || state.InstalledVersion != next {
		t.Fatalf("ApplyLocal = %+v, %v", state, err)
	}
	if version, err := localVersion(ctx, target); err != nil || version != next {
		t.Fatalf("installed version = %q, %v", version, err)
	}
	if _, err := Rollback(target, stateDir); err != nil {
		t.Fatal(err)
	}
	if version, err := localVersion(ctx, target); err != nil || version != current {
		t.Fatalf("restored version = %q, %v", version, err)
	}
}
