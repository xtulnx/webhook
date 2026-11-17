package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	"github.com/xtulnx/webhook/internal/hook"
)

var (
	loadedHooksMu sync.RWMutex

	errAdminReadOnly   = errors.New("admin writes are disabled when -template is enabled")
	errUnknownHookFile = errors.New("unknown hooks file")
)

type adminHooksFile struct {
	Path     string     `json:"path"`
	Format   string     `json:"format"`
	Writable bool       `json:"writable"`
	Hooks    hook.Hooks `json:"hooks"`
}

type adminHooksState struct {
	Files          []adminHooksFile `json:"files"`
	ReadOnly       bool             `json:"readOnly"`
	ReadOnlyReason string           `json:"readOnlyReason,omitempty"`
}

func adminWritesDisabled() bool {
	return *asTemplate
}

func adminReadOnlyReason() string {
	if !adminWritesDisabled() {
		return ""
	}

	return "editing is disabled while webhook is running with -template because the original template source cannot be preserved safely"
}

func cloneHook(src hook.Hook) hook.Hook {
	dst := src

	if src.ResponseHeaders != nil {
		dst.ResponseHeaders = append(hook.ResponseHeaders(nil), src.ResponseHeaders...)
	}
	if src.PassEnvironmentToCommand != nil {
		dst.PassEnvironmentToCommand = append([]hook.Argument(nil), src.PassEnvironmentToCommand...)
	}
	if src.PassArgumentsToCommand != nil {
		dst.PassArgumentsToCommand = append([]hook.Argument(nil), src.PassArgumentsToCommand...)
	}
	if src.PassFileToCommand != nil {
		dst.PassFileToCommand = append([]hook.Argument(nil), src.PassFileToCommand...)
	}
	if src.JSONStringParameters != nil {
		dst.JSONStringParameters = append([]hook.Argument(nil), src.JSONStringParameters...)
	}
	if src.HTTPMethods != nil {
		dst.HTTPMethods = append([]string(nil), src.HTTPMethods...)
	}
	if src.MaxConcurrency != nil {
		value := *src.MaxConcurrency
		dst.MaxConcurrency = &value
	}

	return dst
}

func cloneHooks(src hook.Hooks) hook.Hooks {
	if src == nil {
		return nil
	}

	dst := make(hook.Hooks, len(src))
	for i := range src {
		dst[i] = cloneHook(src[i])
	}

	return dst
}

func cloneLoadedHooksMapLocked() map[string]hook.Hooks {
	dst := make(map[string]hook.Hooks, len(loadedHooksFromFiles))
	for path, hooksInFile := range loadedHooksFromFiles {
		dst[path] = cloneHooks(hooksInFile)
	}

	return dst
}

func hooksFilesSnapshot() []string {
	loadedHooksMu.RLock()
	defer loadedHooksMu.RUnlock()

	return append([]string(nil), []string(hooksFiles)...)
}

func hooksStateSnapshot() adminHooksState {
	loadedHooksMu.RLock()
	files := append([]string(nil), []string(hooksFiles)...)
	loaded := cloneLoadedHooksMapLocked()
	loadedHooksMu.RUnlock()

	state := adminHooksState{
		ReadOnly:       adminWritesDisabled(),
		ReadOnlyReason: adminReadOnlyReason(),
		Files:          make([]adminHooksFile, 0, len(files)),
	}

	for _, path := range files {
		hooksInFile := cloneHooks(loaded[path])
		if hooksInFile == nil {
			hooksInFile = hook.Hooks{}
		}
		sort.Slice(hooksInFile, func(i, j int) bool {
			return hooksInFile[i].ID < hooksInFile[j].ID
		})

		state.Files = append(state.Files, adminHooksFile{
			Path:     path,
			Format:   hooksFileFormat(path),
			Writable: !state.ReadOnly,
			Hooks:    hooksInFile,
		})
	}

	return state
}

func hooksFileFormat(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return "yaml"
	default:
		return "json"
	}
}

func marshalHooksFile(path string, hooksInFile hook.Hooks) ([]byte, error) {
	var (
		data []byte
		err  error
	)

	switch hooksFileFormat(path) {
	case "yaml":
		data, err = yaml.Marshal(hooksInFile)
	default:
		data, err = json.MarshalIndent(hooksInFile, "", "  ")
	}
	if err != nil {
		return nil, err
	}

	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}

	return data, nil
}

func writeHooksFile(path string, hooksInFile hook.Hooks) error {
	data, err := marshalHooksFile(path, hooksInFile)
	if err != nil {
		return err
	}

	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && !info.Mode().IsRegular() {
		return errors.New("Hook target must be a regular file")
	}
	return writeHookReplacement(path, data, info)
}

func validateUniqueHookIDs(candidate map[string]hook.Hooks) error {
	seen := make(map[string]string)

	for path, hooksInFile := range candidate {
		for _, currentHook := range hooksInFile {
			id := strings.TrimSpace(currentHook.ID)
			if id == "" {
				return fmt.Errorf("hook id is required for hooks file %s", path)
			}

			if prevPath, ok := seen[id]; ok {
				return fmt.Errorf("hook with ID %s is already defined in %s", id, prevPath)
			}

			seen[id] = path
		}
	}

	return nil
}

func hookIndexByID(hooksInFile hook.Hooks, id string) int {
	for i := range hooksInFile {
		if hooksInFile[i].ID == id {
			return i
		}
	}

	return -1
}

func normalizeAdminHook(current hook.Hook) hook.Hook {
	current.ID = strings.TrimSpace(current.ID)
	current.ExecuteCommand = strings.TrimSpace(current.ExecuteCommand)
	current.CommandWorkingDirectory = strings.TrimSpace(current.CommandWorkingDirectory)
	current.CommandTimeout = strings.TrimSpace(current.CommandTimeout)
	current.ResponseMessage = strings.TrimSpace(current.ResponseMessage)

	if len(current.HTTPMethods) != 0 {
		methods := make([]string, 0, len(current.HTTPMethods))
		for _, method := range current.HTTPMethods {
			method = strings.ToUpper(strings.TrimSpace(method))
			if method != "" {
				methods = append(methods, method)
			}
		}
		current.HTTPMethods = methods
	}

	return current
}

func upsertHookInFile(path, currentID string, updated hook.Hook) error {
	if adminWritesDisabled() {
		return errAdminReadOnly
	}

	updated = normalizeAdminHook(updated)
	if updated.ID == "" {
		return errors.New("hook id is required")
	}
	if err := updated.ValidateExecutionSettings(); err != nil {
		return err
	}

	return mutateAdminHookFile(path, func(existing hook.Hooks) (hook.Hooks, error) {
		next := cloneHooks(existing)
		if currentID == "" {
			if hookIndexByID(next, updated.ID) >= 0 {
				return nil, fmt.Errorf("hook with ID %s is already defined", updated.ID)
			}
			return append(next, updated), nil
		}
		index := hookIndexByID(next, currentID)
		if index < 0 {
			return nil, fmt.Errorf("hook with ID %s was not found in %s", currentID, path)
		}
		next[index] = updated
		return next, nil
	})
}

func deleteHookFromFile(path, id string) error {
	if adminWritesDisabled() {
		return errAdminReadOnly
	}
	return mutateAdminHookFile(path, func(existing hook.Hooks) (hook.Hooks, error) {
		index := hookIndexByID(existing, id)
		if index < 0 {
			return nil, fmt.Errorf("hook with ID %s was not found in %s", id, path)
		}
		next := cloneHooks(existing)
		next = append(next[:index], next[index+1:]...)
		return next, nil
	})
}

func mutateAdminHookFile(path string, mutate func(hook.Hooks) (hook.Hooks, error)) error {
	loadedHooksMu.RLock()
	paths := make([]string, 0, len(loadedHooksFromFiles))
	for p := range loadedHooksFromFiles {
		paths = append(paths, p)
	}
	_, known := loadedHooksFromFiles[path]
	loadedHooksMu.RUnlock()
	if !known {
		return fmt.Errorf("%w: %s", errUnknownHookFile, path)
	}
	lockPaths := append([]string(nil), paths...)
	for _, dir := range hooksDirectories {
		lockPaths = append(lockPaths, filepath.Join(dir, ".sources"))
	}
	unlock, err := lockHookDirectories(lockPaths)
	if err != nil {
		return err
	}
	defer unlock()
	hookApplyMu.Lock()
	defer hookApplyMu.Unlock()
	loadedHooksMu.Lock()
	defer loadedHooksMu.Unlock()
	if len(paths) != len(loadedHooksFromFiles) {
		return errors.New("Hook sources changed concurrently")
	}
	for _, p := range paths {
		if _, ok := loadedHooksFromFiles[p]; !ok {
			return errors.New("Hook sources changed concurrently")
		}
	}
	sourceSet := map[string]bool{}
	for _, p := range paths {
		sourceSet[p] = true
	}
	for _, dir := range hooksDirectories {
		found, e := directoryHooksFiles(dir)
		if e != nil {
			return e
		}
		for _, p := range found {
			if !sourceSet[p] {
				paths = append(paths, p)
				sourceSet[p] = true
			}
		}
	}
	disk := make([]hookDiskSnapshot, 0, len(paths))
	candidate := map[string]hook.Hooks{}
	for _, p := range paths {
		s, e := readHookSnapshot(p)
		if e != nil {
			return e
		}
		disk = append(disk, s)
		candidate[p] = s.hooks
	}
	next, err := mutate(candidate[path])
	if err != nil {
		return err
	}
	candidate[path] = next
	if err = validateUniqueHookIDs(candidate); err != nil {
		return err
	}
	running := cloneLoadedHooksMapLocked()
	running[path] = next
	if err = validateUniqueHookIDs(running); err != nil {
		return err
	}
	if _, err = commitHookSnapshot(disk, path, next); err != nil {
		return err
	}
	loadedHooksFromFiles[path] = next
	markHookLoadedLocked(path, time.Now())
	return nil
}
