package network

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

func validPrefixPath(value string) bool {
	value = strings.TrimSuffix(value, "/")
	return validRoutePath(value) && !strings.Contains(value, "%") && strings.IndexFunc(value, unicode.IsSpace) < 0
}

func normalizePrefix(value, fallback string) (string, error) {
	if value == "" {
		return fallback, nil
	}
	if !validPrefixPath(value) {
		return "", errors.New("must be an absolute path prefix with valid segments")
	}
	return strings.TrimSuffix(value, "/"), nil
}

func transportPath(prefix, endpoint string) string {
	if prefix == "" {
		prefix = DefaultTransportPrefix
	}
	return prefix + endpoint
}

// PathsOverlap compares canonical paths at segment boundaries.
func PathsOverlap(a, b string) bool {
	return a == "/" || b == "/" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func (c *HubConfig) ReservedRoutes() []string {
	transport := c.TransportPrefix
	if transport == "" || transport == DefaultTransportPrefix {
		transport = "/__webhook_network"
	}
	share := c.SharePrefix
	if share == "" {
		share = strings.TrimSuffix(SharePath, "/")
	}
	return []string{transport, share}
}

func (r HookRoute) Mount() string {
	if r.Prefix != "" {
		return r.Prefix
	}
	return r.Path
}

func validateHookRoutes(cfg *HubConfig, nodes map[string]*NodePolicy) error {
	var allowed []string
	for id, node := range nodes {
		if contains(node.Services, "webhook") {
			allowed = append(allowed, id)
		}
	}
	sort.Strings(allowed)
	groups := make(map[string][]string, len(cfg.AgentGroups))
	groupNames := make([]string, 0, len(cfg.AgentGroups))
	for name := range cfg.AgentGroups {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	for _, name := range groupNames {
		selectors := cfg.AgentGroups[name]
		if !validName(name) || len(selectors) == 0 {
			return fmt.Errorf("invalid or empty agent group %q", name)
		}
		targets, err := resolveAgentSelectors(selectors, allowed)
		if err != nil {
			return fmt.Errorf("agent group %q: %w", name, err)
		}
		groups[name] = targets
	}

	paths, prefixes := map[string]bool{}, map[string]bool{}
	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		if route.Prefix != "" {
			prefix, err := normalizePrefix(route.Prefix, "")
			if err != nil || route.Path != "" || route.Hook != "" || prefixes[prefix] {
				return fmt.Errorf("invalid or duplicate hook route prefix %q", route.Prefix)
			}
			route.Prefix = prefix
			prefixes[prefix] = true
			seen := map[string]bool{}
			for _, hook := range route.Hooks {
				if !ValidHookID(hook) || seen[hook] {
					return fmt.Errorf("hook route %q has invalid or duplicate hook ID %q", prefix, hook)
				}
				seen[hook] = true
			}
		} else {
			if !validRoutePath(route.Path) || !ValidHookID(route.Hook) || route.Hooks != nil || paths[route.Path] {
				return fmt.Errorf("invalid or duplicate hook route %q", route.Path)
			}
			paths[route.Path] = true
		}
		mount := route.Mount()
		for _, reserved := range cfg.ReservedRoutes() {
			if PathsOverlap(mount, reserved) {
				return fmt.Errorf("hook route %q overlaps reserved path %q", mount, reserved)
			}
		}
		targets, err := resolveAgentSelectors(route.Agents, allowed)
		if err != nil {
			return fmt.Errorf("hook route %q: %w", mount, err)
		}
		for _, group := range route.Groups {
			ids, ok := groups[group]
			if !ok {
				return fmt.Errorf("hook route %q refers to unknown agent group %q", mount, group)
			}
			targets = append(targets, ids...)
		}
		if len(targets) == 0 {
			return fmt.Errorf("hook route %q requires agents or groups", mount)
		}
		route.targets = distinctTargets(targets)
		if len(route.AllowIPs) > 0 && len(route.DenyIPs) > 0 {
			return fmt.Errorf("hook route %q cannot use both allow_ips and deny_ips", mount)
		}
		route.allowNets, err = parseNetworks(route.AllowIPs)
		if err != nil {
			return fmt.Errorf("hook route %q allow_ips: %w", mount, err)
		}
		route.denyNets, err = parseNetworks(route.DenyIPs)
		if err != nil {
			return fmt.Errorf("hook route %q deny_ips: %w", mount, err)
		}
	}
	return nil
}

func resolveAgentSelectors(selectors, allowed []string) ([]string, error) {
	var targets []string
	for _, selector := range selectors {
		before := len(targets)
		match, err := agentSelector(selector)
		if err != nil {
			return nil, fmt.Errorf("invalid agent selector %q: %w", selector, err)
		}
		for _, id := range allowed {
			if match(id) {
				targets = append(targets, id)
			}
		}
		if len(targets) == before {
			return nil, fmt.Errorf("agent selector %q matches no node offering webhook", selector)
		}
	}
	return targets, nil
}

func agentSelector(selector string) (func(string) bool, error) {
	if pattern, ok := strings.CutPrefix(selector, "re:"); ok {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		re.Longest()
		return func(id string) bool {
			match := re.FindStringIndex(id)
			return match != nil && match[0] == 0 && match[1] == len(id)
		}, nil
	}
	if _, err := path.Match(selector, ""); err != nil {
		return nil, err
	}
	return func(id string) bool {
		matched, _ := path.Match(selector, id)
		return matched
	}, nil
}

func distinctTargets(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result
}
