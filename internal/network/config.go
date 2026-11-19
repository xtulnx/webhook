package network

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v2"
)

const (
	DefaultTransportPrefix = "/__webhook_network/v1"
	ControlPath            = DefaultTransportPrefix + "/control"
	DataPath               = DefaultTransportPrefix + "/data"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type Config struct {
	Mode  string       `yaml:"mode"`
	Hub   *HubConfig   `yaml:"hub,omitempty"`
	Agent *AgentConfig `yaml:"agent,omitempty"`
}

type HubConfig struct {
	TransportPrefix string              `yaml:"transport_prefix"`
	SharePrefix     string              `yaml:"share_prefix"`
	SharePublicURL  string              `yaml:"share_public_url"`
	AgentGroups     map[string][]string `yaml:"agent_groups"`
	RequestTimeout  time.Duration       `yaml:"request_timeout"`
	Nodes           []NodePolicy        `yaml:"nodes"`
	MaxSessions     int                 `yaml:"max_sessions"`
	Routes          []HookRoute         `yaml:"hook_routes"`
}

type HookRoute struct {
	Path      string   `yaml:"path"`
	Prefix    string   `yaml:"prefix"`
	Hook      string   `yaml:"hook"`
	Hooks     []string `yaml:"hooks"`
	Agents    []string `yaml:"agents"`
	Groups    []string `yaml:"groups"`
	AllowIPs  []string `yaml:"allow_ips"`
	DenyIPs   []string `yaml:"deny_ips"`
	allowNets []*net.IPNet
	denyNets  []*net.IPNet
	targets   []string
}

type NodePolicy struct {
	ID              string   `yaml:"id"`
	Token           string   `yaml:"token"`
	TokenEnv        string   `yaml:"token_env"`
	TokenFile       string   `yaml:"token_file"`
	Services        []string `yaml:"services"`
	Allow           []string `yaml:"allow"`
	AllowShareLinks bool     `yaml:"allow_share_links"`
	HookPrefix      string   `yaml:"hook_prefix"`
	tokenHash       [32]byte
}

type AgentConfig struct {
	Server            string    `yaml:"server"`
	TransportPrefix   string    `yaml:"transport_prefix"`
	ID                string    `yaml:"id"`
	Token             string    `yaml:"token"`
	TokenEnv          string    `yaml:"token_env"`
	TokenFile         string    `yaml:"token_file"`
	CAFile            string    `yaml:"ca_file"`
	Services          []Service `yaml:"services"`
	Forwards          []Forward `yaml:"forwards"`
	AllowPublicListen bool      `yaml:"allow_public_listen"`
	MaxConnections    int       `yaml:"max_connections"`
	WithWebhook       bool      `yaml:"with_webhook"`
	WebhookOnlyViaHub bool      `yaml:"webhook_only_via_hub"`
	token             string
}

type Service struct {
	InProcess bool   `yaml:"-"`
	Name      string `yaml:"name"`
	Target    string `yaml:"target"`
	RelayTo   string `yaml:"relay_to"`
}

type Forward struct {
	Listen string `yaml:"listen"`
	To     string `yaml:"to"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read network config: %w", err)
	}
	return ParseConfig(data, filepath.Dir(path))
}

// ParseConfig loads an embedded network configuration. Relative token and CA
// paths are resolved from the directory of the containing YAML file.
func ParseConfig(data []byte, base string) (*Config, error) {
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse network config: %w", err)
	}
	switch cfg.Mode {
	case "hub":
		if cfg.Hub == nil || cfg.Agent != nil {
			return nil, errors.New("hub mode requires hub settings only")
		}
		if err := validateHub(cfg.Hub, base); err != nil {
			return nil, err
		}
	case "agent":
		if cfg.Agent == nil || cfg.Hub != nil {
			return nil, errors.New("agent mode requires agent settings only")
		}
		if err := validateAgent(cfg.Agent, base); err != nil {
			return nil, err
		}
		for _, service := range cfg.Agent.Services {
			if service.RelayTo != "" {
				return nil, errors.New("relay_to requires both mode")
			}
		}
	case "both":
		if cfg.Hub == nil || cfg.Agent == nil {
			return nil, errors.New("both mode requires hub and agent settings")
		}
		if cfg.Agent.WebhookOnlyViaHub {
			return nil, errors.New("webhook_only_via_hub requires agent mode")
		}
		if err := validateHub(cfg.Hub, base); err != nil {
			return nil, err
		}
		if err := validateAgent(cfg.Agent, base); err != nil {
			return nil, err
		}
		for _, service := range cfg.Agent.Services {
			if service.RelayTo == "" {
				continue
			}
			id, name, _ := splitDestination(service.RelayTo)
			if !hubHasService(cfg.Hub, id, name) {
				return nil, fmt.Errorf("relay_to %q is not declared by the local hub", service.RelayTo)
			}
		}
	default:
		return nil, errors.New("network mode must be hub, agent, or both")
	}
	return &cfg, nil
}

func validateHub(cfg *HubConfig, base string) error {
	var err error
	cfg.TransportPrefix, err = normalizePrefix(cfg.TransportPrefix, DefaultTransportPrefix)
	if err != nil {
		return fmt.Errorf("transport_prefix: %w", err)
	}
	cfg.SharePrefix, err = normalizePrefix(cfg.SharePrefix, strings.TrimSuffix(SharePath, "/"))
	if err != nil {
		return fmt.Errorf("share_prefix: %w", err)
	}
	if PathsOverlap(cfg.SharePrefix, cfg.TransportPrefix+"/control") || PathsOverlap(cfg.SharePrefix, cfg.TransportPrefix+"/data") {
		return errors.New("share_prefix overlaps the transport control or data path")
	}
	if cfg.SharePublicURL != "" {
		u, err := url.Parse(cfg.SharePublicURL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.SharePublicURL, "#") ||
			(u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname()))) {
			return errors.New("share_public_url must be an HTTPS base URL without credentials, query, or fragment (HTTP only on loopback)")
		}
		if !validPrefixPath(u.Path) {
			return errors.New("share_public_url must include a valid download path")
		}
		cfg.SharePublicURL = strings.TrimRight(cfg.SharePublicURL, "/")
	}
	if len(cfg.Nodes) == 0 {
		return errors.New("hub requires at least one node")
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 60 * time.Second
	}
	if cfg.RequestTimeout < 0 {
		return errors.New("request_timeout must be positive")
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 128
	}
	if cfg.MaxSessions < 1 {
		return errors.New("max_sessions must be positive")
	}
	nodes := make(map[string]*NodePolicy)
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		if !validName(n.ID) {
			return fmt.Errorf("invalid node id %q", n.ID)
		}
		if _, exists := nodes[n.ID]; exists {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		token, err := resolveToken(base, n.Token, n.TokenEnv, n.TokenFile)
		if err != nil {
			return fmt.Errorf("node %q: %w", n.ID, err)
		}
		n.tokenHash = sha256.Sum256([]byte(token))
		if n.HookPrefix == "" {
			n.HookPrefix = "/hooks"
		}
		if !validRoutePath(n.HookPrefix) {
			return fmt.Errorf("node %q has invalid hook_prefix %q", n.ID, n.HookPrefix)
		}
		seen := make(map[string]bool)
		for _, service := range n.Services {
			if !validName(service) || seen[service] {
				return fmt.Errorf("node %q has invalid or duplicate service %q", n.ID, service)
			}
			seen[service] = true
		}
		if n.AllowShareLinks && !seen["files"] {
			return fmt.Errorf("node %q: allow_share_links requires the files service", n.ID)
		}
		nodes[n.ID] = n
	}
	for _, n := range cfg.Nodes {
		seen := make(map[string]bool)
		for _, dest := range n.Allow {
			id, service, ok := splitDestination(dest)
			if service == "webhook" {
				return fmt.Errorf("node %q: webhook cannot be granted through allow; use hook_routes", n.ID)
			}
			if !ok || nodes[id] == nil || !contains(nodes[id].Services, service) || seen[dest] {
				return fmt.Errorf("node %q has invalid or duplicate allow target %q", n.ID, dest)
			}
			seen[dest] = true
		}
	}
	return validateHookRoutes(cfg, nodes)
}

func validateAgent(cfg *AgentConfig, base string) error {
	var err error
	cfg.TransportPrefix, err = normalizePrefix(cfg.TransportPrefix, DefaultTransportPrefix)
	if err != nil {
		return fmt.Errorf("transport_prefix: %w", err)
	}
	if !validName(cfg.ID) {
		return fmt.Errorf("invalid node id %q", cfg.ID)
	}
	if cfg.WebhookOnlyViaHub && !cfg.WithWebhook {
		return errors.New("webhook_only_via_hub requires with_webhook")
	}
	u, err := url.Parse(cfg.Server)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.Server, "#") {
		return errors.New("server must be a ws:// or wss:// base URL without credentials, query, or fragment")
	}
	if u.Scheme != "wss" && (u.Scheme != "ws" || !isLoopbackHost(u.Hostname())) {
		return errors.New("server must use wss:// (ws:// is allowed only for loopback)")
	}
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	cfg.token, err = resolveToken(base, cfg.Token, cfg.TokenEnv, cfg.TokenFile)
	if err != nil {
		return err
	}
	if cfg.CAFile != "" && !filepath.IsAbs(cfg.CAFile) {
		cfg.CAFile = filepath.Join(base, cfg.CAFile)
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 64
	}
	if cfg.MaxConnections < 1 {
		return errors.New("max_connections must be positive")
	}
	services := make(map[string]bool)
	for _, service := range cfg.Services {
		if !validName(service.Name) || services[service.Name] {
			return fmt.Errorf("invalid or duplicate service %q", service.Name)
		}
		if cfg.WebhookOnlyViaHub && service.Name == "webhook" {
			return errors.New("webhook_only_via_hub cannot set a webhook service target")
		}
		if service.InProcess {
			if service.Target != "" || service.RelayTo != "" {
				return errors.New("in-process service cannot set a target")
			}
			services[service.Name] = true
			continue
		}
		if (service.Target == "") == (service.RelayTo == "") {
			return fmt.Errorf("service %q requires exactly one of target or relay_to", service.Name)
		}
		if service.Target != "" {
			if err := validateAddress(service.Target); err != nil {
				return fmt.Errorf("service %q target: %w", service.Name, err)
			}
		} else if _, _, ok := splitDestination(service.RelayTo); !ok {
			return fmt.Errorf("service %q has invalid relay_to %q", service.Name, service.RelayTo)
		}
		services[service.Name] = true
	}
	if cfg.WebhookOnlyViaHub {
		cfg.Services = append(cfg.Services, Service{Name: "webhook"})
	}
	forwards := make(map[string]bool)
	for _, forward := range cfg.Forwards {
		if err := validateAddress(forward.Listen); err != nil {
			return fmt.Errorf("forward listen: %w", err)
		}
		host, _, _ := net.SplitHostPort(forward.Listen)
		if !cfg.AllowPublicListen && !isLoopbackHost(host) {
			return fmt.Errorf("forward %q must listen on loopback unless allow_public_listen is true", forward.Listen)
		}
		if _, _, ok := splitDestination(forward.To); !ok {
			return fmt.Errorf("invalid forward destination %q", forward.To)
		}
		if forwards[forward.Listen] {
			return fmt.Errorf("duplicate forward listen %q", forward.Listen)
		}
		forwards[forward.Listen] = true
	}
	return nil
}

func hubHasService(cfg *HubConfig, id, service string) bool {
	for _, node := range cfg.Nodes {
		if node.ID == id && contains(node.Services, service) {
			return true
		}
	}
	return false
}

func validRoutePath(path string) bool {
	if !strings.HasPrefix(path, "/") || path == "/" || strings.ContainsAny(path, "?#{}\\") || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func parseNetworks(values []string) ([]*net.IPNet, error) {
	var result []*net.IPNet
	for _, value := range values {
		if !strings.Contains(value, "/") {
			if ip := net.ParseIP(value); ip == nil {
				return nil, fmt.Errorf("invalid IP %q", value)
			} else if ip.To4() != nil {
				value += "/32"
			} else {
				value += "/128"
			}
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, err
		}
		result = append(result, network)
	}
	return result, nil
}

func readToken(base, name string) (string, error) {
	if name == "" {
		return "", errors.New("token_file is required")
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(base, name)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read token_file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("token_file must contain one token of at least 32 characters")
	}
	return token, nil
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return fmt.Errorf("%q must be host:port", address)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q has invalid port", address)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

func validName(s string) bool { return namePattern.MatchString(s) }

func ValidShareName(s string) bool { return validName(s) }

func ValidHookID(s string) bool {
	if strings.ContainsRune(s, '\\') || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return false
	}
	for _, segment := range strings.Split(s, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func splitDestination(s string) (string, string, bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Resolve credentials without echoing secret material in validation errors.
func resolveToken(base, value, env, file string) (string, error) {
	count := 0
	for _, v := range []string{value, env, file} {
		if v != "" {
			count++
		}
	}
	if count != 1 {
		return "", errors.New("set exactly one of token, token_env, token_file")
	}
	if file != "" {
		return readToken(base, file)
	}
	if env != "" {
		value = os.Getenv(env)
	}
	if len(value) < 32 || strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("token must contain at least 32 characters without whitespace (check token_env if used)")
	}
	return value, nil
}

// ValidateAgent applies the same policy to a temporary CLI agent as a YAML agent.
func ValidateAgent(cfg *AgentConfig) error { return validateAgent(cfg, ".") }
