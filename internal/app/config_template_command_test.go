package app

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/xtulnx/webhook/internal/hook"
	"github.com/xtulnx/webhook/internal/network"
)

func TestConfigurationTemplatesParseAndCoverSchemas(t *testing.T) {
	t.Setenv("WEBHOOK_NODE_TOKEN", strings.Repeat("n", 32))
	t.Setenv("WEBHOOK_UPSTREAM_TOKEN", strings.Repeat("u", 32))
	var allTemplates strings.Builder
	for _, item := range templateCatalog {
		for _, format := range []string{"yaml", "json"} {
			t.Run(item.Name+"/"+format, func(t *testing.T) {
				data, e := renderConfigTemplate(item.Name, format)
				if e != nil {
					t.Fatal(e)
				}
				if format == "yaml" {
					if !bytes.Contains(data, []byte("#")) {
						t.Fatal("YAML lacks guidance")
					}
					allTemplates.Write(data)
				} else if !json.Valid(data) {
					t.Fatal("invalid JSON")
				}
				switch item.Name {
				case "hook", "hooks":
					var hs hook.Hooks
					if item.Name == "hook" {
						data = []byte("-\n" + indentTemplate(string(data)))
					}
					if e = hs.LoadFromBytesStrict(data); e != nil {
						t.Fatal(e)
					}
					if len(hs) != 1 || hs[0].ExecuteCommand != "./replace-with-your-command" {
						t.Fatal("unsafe or missing example")
					}
					if e = validateTestRules(hs[0].TriggerRule); e != nil {
						t.Fatal(e)
					}
				case "network-hub", "network-agent", "network-both":
					if _, e = network.ParseConfig(data, t.TempDir()); e != nil {
						t.Fatal(e)
					}
				case "context":
					raw, e := yaml.YAMLToJSON(data)
					if e != nil {
						t.Fatal(e)
					}
					dec := json.NewDecoder(bytes.NewReader(raw))
					dec.DisallowUnknownFields()
					var c testContext
					if e = dec.Decode(&c); e != nil {
						t.Fatal(e)
					}
				default:
					var cfg map[string]interface{}
					if e = yaml.Unmarshal(data, &cfg); e != nil {
						t.Fatal(e)
					}
					for key := range cfg {
						if key != "hooks" && key != "network" && flag.CommandLine.Lookup(key) == nil {
							t.Fatal("unknown global key", key)
						}
					}
					if item.Name == "global" && cfg["ip"] != "127.0.0.1" {
						t.Fatal("template exposes public listener")
					}
					if n, ok := cfg["network"]; ok {
						raw, e := yaml.Marshal(n)
						if e != nil {
							t.Fatal(e)
						}
						if _, e = network.ParseConfig(raw, t.TempDir()); e != nil {
							t.Fatal(e)
						}
					}
				}
			})
		}
	}
	// Every configurable Hook/network/context field must be discoverable in the templates,
	// including optional features that stay commented out for safe initial deployment.
	for _, sample := range []interface{}{hook.Hook{}, hook.Argument{}, network.HubConfig{}, network.AgentConfig{}, network.NodePolicy{}, network.HookRoute{}, network.Service{}, network.Forward{}, testContext{}, testUpload{}} {
		typ := reflect.TypeOf(sample)
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := f.Tag.Get("json")
			if tag == "" {
				tag = f.Tag.Get("yaml")
			}
			name := strings.Split(tag, ",")[0]
			if name == "" || name == "-" {
				continue
			}
			if !strings.Contains(allTemplates.String(), name+":") {
				t.Errorf("template guidance missing %s.%s (%s)", typ.Name(), f.Name, name)
			}
		}
	}
}

func TestTemplateCommandFileSafetyAndOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	var out, errs bytes.Buffer
	code := runConfigCommand([]string{"template", "hooks", "--output", path}, &out, &errs)
	if code != 0 || out.Len() != 0 {
		t.Fatalf("%d %s %s", code, out.String(), errs.String())
	}
	before, e := os.ReadFile(path)
	if e != nil || !json.Valid(before) {
		t.Fatal("extension inference failed", e)
	}
	if code = runConfigCommand([]string{"template", "hooks", "--output", path}, &out, &errs); code != 1 {
		t.Fatal("overwrote existing file")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("source changed")
	}
	if code = runConfigCommand([]string{"template", "hooks", "--format", "yaml", "--output", path}, &out, &errs); code != 2 {
		t.Fatal("format conflict not rejected")
	}
	out.Reset()
	if code = runConfigCommand([]string{"template", "list", "--json"}, &out, &errs); code != 0 {
		t.Fatal(code, errs.String())
	}
	var catalog []configTemplateInfo
	if e = json.Unmarshal(out.Bytes(), &catalog); e != nil || len(catalog) != len(templateCatalog) {
		t.Fatal("catalog invalid", e)
	}
	out.Reset()
	if code = runConfigCommand([]string{"template", "typo"}, &out, &errs); code != 2 || out.Len() != 0 {
		t.Fatal("unknown type accepted")
	}
	for _, removed := range []string{"--interactive", "--apply"} {
		out.Reset()
		if code = runHookWriteCommand("add", []string{removed}, &out, &errs); code != 2 || out.Len() != 0 {
			t.Fatal("removed wizard flag accepted")
		}
	}
}

func TestConfigInitProfilesAndFormats(t *testing.T) {
	t.Setenv("WEBHOOK_NODE_TOKEN", strings.Repeat("n", 32))
	t.Setenv("WEBHOOK_UPSTREAM_TOKEN", strings.Repeat("u", 32))
	for _, profile := range []string{"global", "hub", "agent", "both"} {
		for _, format := range []string{"yaml", "json"} {
			t.Run(profile+"/"+format, func(t *testing.T) {
				target := filepath.Join(t.TempDir(), "new")
				var out, errs bytes.Buffer
				if code := runConfigCommand([]string{"init", "--dir", target, "--profile", profile, "--format", format}, &out, &errs); code != 0 {
					t.Fatal(code, errs.String())
				}
				cfg := filepath.Join(target, "webhook."+format)
				raw, e := os.ReadFile(cfg)
				if e != nil {
					t.Fatal(e)
				}
				if format == "yaml" && !bytes.Contains(raw, []byte("#")) {
					t.Fatal("init stripped comments")
				}
				files, e := readLocalHooks(localHookOptions{Config: cfg})
				if e != nil || len(files) != 1 || len(files[0].Hooks) != 0 {
					t.Fatalf("generated sources invalid %+v %v", files, e)
				}
				var settings map[string]interface{}
				if e = yaml.Unmarshal(raw, &settings); e != nil {
					t.Fatal(e)
				}
				if paths := settings["hooks"].([]interface{}); paths[0] != filepath.Join(target, "hooks."+format) {
					t.Fatal("init path incorrect", paths)
				}
				if settings["ip"] != "127.0.0.1" {
					t.Fatal("init missing global defaults")
				}
				expectedPort := float64(9000)
				if profile == "both" {
					expectedPort = 9001
				}
				if settings["port"] != expectedPort {
					t.Fatal("init port changed", settings["port"])
				}
				if profile != "global" {
					n, e := yaml.Marshal(settings["network"])
					if e != nil {
						t.Fatal(e)
					}
					if _, e = network.ParseConfig(n, target); e != nil {
						t.Fatal(e)
					}
				}
			})
		}
	}
}

// Exercise the CLI and resulting schema for every supported module combination.
func TestTemplateModuleCombinations(t *testing.T) {
	t.Setenv("WEBHOOK_NODE_TOKEN", strings.Repeat("n", 32))
	t.Setenv("WEBHOOK_UPSTREAM_TOKEN", strings.Repeat("u", 32))
	for _, mode := range []string{"", "hub", "agent", "both"} {
		for mask := 0; mask < 8; mask++ {
			names := []string{}
			if mode != "" {
				names = append(names, mode)
			}
			for i, name := range []string{"global", "admin", "tls"} {
				if mask&(1<<i) != 0 {
					names = append(names, name)
				}
			}
			if len(names) == 0 {
				continue
			}
			t.Run(strings.Join(names, "+"), func(t *testing.T) {
				var yamlFields map[string]interface{}
				for _, format := range []string{"yaml", "json"} {
					var out, errs bytes.Buffer
					args := append([]string{"template"}, names...)
					args = append(args, "--format", format)
					if code := runConfigCommand(args, &out, &errs); code != 0 {
						t.Fatal(code, errs.String())
					}
					var fields map[string]interface{}
					if e := yaml.Unmarshal(out.Bytes(), &fields); e != nil {
						t.Fatal(e)
					}
					if format == "yaml" {
						yamlFields = fields
						if !bytes.Contains(out.Bytes(), []byte("#")) {
							t.Fatal("comments lost")
						}
					} else if !json.Valid(out.Bytes()) || !reflect.DeepEqual(fields, yamlFields) {
						t.Fatal("JSON and YAML differ")
					}
					_, hasIP := fields["ip"]
					_, hasHooks := fields["hooks"]
					if hasIP != (mask&1 != 0) || hasHooks != hasIP {
						t.Fatal("global fields leaked or missing", fields)
					}
					if mask&1 != 0 && fields["port"] != float64(9000) {
						t.Fatal("module changed global port")
					}
					_, hasAdmin := fields["admin"]
					_, hasTLS := fields["secure"]
					if hasAdmin != (mask&2 != 0) || hasTLS != (mask&4 != 0) {
						t.Fatal("module fields leaked or missing")
					}
					if mode != "" {
						raw, e := yaml.Marshal(fields["network"])
						if e != nil {
							t.Fatal(e)
						}
						if _, e := network.ParseConfig(raw, t.TempDir()); e != nil {
							t.Fatal(e)
						}
					} else if _, ok := fields["network"]; ok {
						t.Fatal("unexpected network")
					}
				}
				reversed := append([]string(nil), names...)
				for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
					reversed[i], reversed[j] = reversed[j], reversed[i]
				}
				a, e := renderConfigTemplates(names, "yaml")
				if e != nil {
					t.Fatal(e)
				}
				b, e := renderConfigTemplates(reversed, "yaml")
				if e != nil || !bytes.Equal(a, b) {
					t.Fatal("module ordering changed output", e)
				}
			})
		}
	}
}

func TestTemplateModuleErrorsDoNotWrite(t *testing.T) {
	for _, names := range [][]string{
		{"hub", "agent"}, {"hub", "both"}, {"agent", "both"},
		{"global", "global"}, {"admin", "admin"}, {"global", "typo"},
		{"global", "hook"}, {"hooks", "global"}, {"context", "admin"},
		{"network-hub", "hub"}, {"network-hub", "network-agent"},
	} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.yaml")
			args := append([]string{"template"}, names...)
			args = append(args, "--output", path)
			var out, errs bytes.Buffer
			if code := runConfigCommand(args, &out, &errs); code != 2 || out.Len() != 0 || errs.Len() == 0 {
				t.Fatal(code, out.String(), errs.String())
			}
			if _, e := os.Stat(path); !os.IsNotExist(e) {
				t.Fatal("invalid combination created output", e)
			}
		})
	}
	var out, errs bytes.Buffer
	if code := runConfigCommand([]string{"template", "global", "--format", "yaml", "admin"}, &out, &errs); code != 2 || out.Len() != 0 {
		t.Fatal("misplaced module accepted")
	}
}
