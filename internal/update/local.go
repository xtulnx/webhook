package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CheckLocal runs the trusted local candidate and installed binary to read their versions.
func CheckLocal(ctx context.Context, source, target string) (Result, error) {
	target, err := resolveTarget(target)
	if err != nil {
		return Result{}, err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return Result{}, err
	}
	return checkLocalVersions(ctx, source, target)
}

func checkLocalVersions(ctx context.Context, source, target string) (Result, error) {
	current, err := localVersion(ctx, target)
	if err != nil {
		return Result{}, err
	}
	candidate, err := localVersion(ctx, source)
	if err != nil {
		return Result{}, err
	}
	return Result{
		CurrentVersion: current,
		LatestVersion:  candidate,
		Available:      CompareVersions(candidate, current) > 0,
		CheckedAt:      time.Now().UTC(),
	}, nil
}

// ApplyLocal explicitly installs a trusted local binary, including a same-version
// build or downgrade. The staged copy is probed before it replaces the target.
func ApplyLocal(ctx context.Context, source, target, stateDir string) (State, error) {
	target, err := resolveTarget(target)
	if err != nil {
		return State{}, err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return State{}, err
	}
	if !sourceInfo.Mode().IsRegular() {
		return State{}, fmt.Errorf("local update requires a regular source file")
	}
	input, err := os.Open(source)
	if err != nil {
		return State{}, err
	}
	defer input.Close()
	sourceInfo, err = input.Stat()
	if err != nil {
		return State{}, err
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return State{}, err
	}
	if !sourceInfo.Mode().IsRegular() || !targetInfo.Mode().IsRegular() {
		return State{}, fmt.Errorf("local update requires regular executable files")
	}
	if os.SameFile(sourceInfo, targetInfo) {
		return State{}, fmt.Errorf("local update source and target are the same file")
	}

	pattern := ".webhook-local-*"
	if runtime.GOOS == "windows" {
		pattern += ".exe"
	}
	staged, err := os.CreateTemp(filepath.Dir(target), pattern)
	if err != nil {
		return State{}, err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(staged, hash), io.LimitReader(input, maxBinarySize+1))
	if err != nil {
		return State{}, err
	}
	if size > maxBinarySize {
		return State{}, fmt.Errorf("local binary exceeds %d bytes", maxBinarySize)
	}
	if err := staged.Chmod(targetInfo.Mode().Perm()); err != nil {
		return State{}, err
	}
	if err := staged.Sync(); err != nil {
		return State{}, err
	}
	if err := staged.Close(); err != nil {
		return State{}, err
	}
	result, err := checkLocalVersions(ctx, staged.Name(), target)
	if err != nil {
		return State{}, err
	}
	stateDir, err = prepareStateDir(stateDir)
	if err != nil {
		return State{}, err
	}
	return installBinary(staged.Name(), target, stateDir, result.CurrentVersion, result.LatestVersion, hex.EncodeToString(hash.Sum(nil)))
}

func localVersion(ctx context.Context, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return "", fmt.Errorf("read webhook version from %s: %w", path, err)
	}
	version, found := strings.CutPrefix(strings.TrimSpace(string(output)), "webhook version ")
	if !found || strings.ContainsAny(version, " \t\r\n") {
		return "", fmt.Errorf("%s did not report a webhook version", path)
	}
	if _, valid := parseVersion(version); !valid {
		return "", fmt.Errorf("%s reported an invalid webhook version %q", path, version)
	}
	return version, nil
}
