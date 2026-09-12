package main

import (
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		Port: "8080", Cookies: "cookies.json", Upstream: "https://example.invalid",
		AliasModel: LegacyModelAlias, Product: "orcaterm", Origin: "http://tauri.localhost",
		UserAgent: "test", UpstreamModel: defaultUpstreamModel, Timeouts: 300,
		Mode: "structured", OnDegraded: "empty", Strip: true, HideTools: true,
		SessionTTL: 30 * time.Minute, SessionMax: 1000, ToolsMode: "native", ContextLimit: 128000,
	}
}

func TestValidateConfigAcceptsDefaults(t *testing.T) {
	if err := validateConfig(validConfig()); err != nil {
		t.Fatalf("默认配置不应报错: %v", err)
	}
	c := validConfig()
	c.Mode = "raw-stream"
	c.ToolsMode = "ignore"
	c.OnDegraded = "raw"
	if err := validateConfig(c); err != nil {
		t.Fatalf("合法取值不应报错: %v", err)
	}
}

func TestValidateConfigRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"mode", func(c *Config) { c.Mode = "streaming" }, "ORCATERM_MODE"},
		{"tools mode", func(c *Config) { c.ToolsMode = "whatever" }, "ORCATERM_TOOLS_MODE"},
		{"on degraded", func(c *Config) { c.OnDegraded = "silent" }, "ORCATERM_ON_DEGRADED"},
		{"timeout", func(c *Config) { c.Timeouts = 0 }, "ORCATERM_TIMEOUT"},
		{"negative timeout", func(c *Config) { c.Timeouts = -1 }, "ORCATERM_TIMEOUT"},
		{"session ttl", func(c *Config) { c.SessionTTL = 0 }, "ORCATERM_SESSION_TTL"},
		{"session max", func(c *Config) { c.SessionMax = 0 }, "ORCATERM_SESSION_MAX"},
		{"context limit", func(c *Config) { c.ContextLimit = 0 }, "ORCATERM_CONTEXT_LIMIT"},
		{"pipe origin", func(c *Config) { c.CORSOrigins = []string{"|"} }, "ORCATERM_CORS_ORIGINS"},
		{"warnings", func(c *Config) { c.ConfigWarnings = []string{"ORCATERM_TIMEOUT must be an integer"} }, "invalid configuration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			c.mutate(&cfg)
			err := validateConfig(cfg)
			if err == nil {
				t.Fatalf("非法配置未被拒绝")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), c.want)
			}
		})
	}
}

func TestNormalizeOrigin(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"*", "*", false},
		{"http://localhost:3000", "http://localhost:3000", false},
		{"  https://app.example.com  ", "https://app.example.com", false},
		{"http://localhost:3000/", "http://localhost:3000", false},
		{"http://localhost:3000/path", "", true},
		{"http://localhost:3000/?q=1", "", true},
		{"ftp://localhost:3000", "", true},
		{"localhost:3000", "", true},
		{"http://", "", true},
	}
	for _, c := range cases {
		got, err := normalizeOrigin(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应被拒绝，实际得到 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 不应报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q -> %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestLoadConfigCORSOrigins(t *testing.T) {
	t.Setenv("ORCATERM_CORS_ORIGINS", "http://localhost:3000/ , https://app.example.com")
	c := loadConfig()
	want := []string{"http://localhost:3000", "https://app.example.com"}
	if len(c.CORSOrigins) != len(want) {
		t.Fatalf("CORSOrigins = %#v，期望 %#v", c.CORSOrigins, want)
	}
	for i := range want {
		if c.CORSOrigins[i] != want[i] {
			t.Errorf("CORSOrigins[%d] = %q，期望 %q", i, c.CORSOrigins[i], want[i])
		}
	}
	if err := validateConfig(c); err != nil {
		t.Fatalf("规范化后的配置应合法: %v", err)
	}
}

func TestLoadConfigRejectsInvalidOriginAndScalars(t *testing.T) {
	t.Setenv("ORCATERM_CORS_ORIGINS", "http://ok.example, not-an-origin")
	t.Setenv("ORCATERM_TIMEOUT", "abc")
	t.Setenv("ORCATERM_SESSION_MAX", "0")
	c := loadConfig()
	if len(c.ConfigWarnings) < 2 {
		t.Fatalf("非法环境变量应产生警告: %#v", c.ConfigWarnings)
	}
	if c.SessionMax != 0 {
		t.Errorf("SessionMax = %d，0 应被保留并由 validateConfig 拒绝", c.SessionMax)
	}
	if err := validateConfig(c); err == nil {
		t.Fatal("非法配置必须在启动前失败")
	}
	// 合法的那个 origin 仍然被保留（规范化后）
	if len(c.CORSOrigins) != 1 || c.CORSOrigins[0] != "http://ok.example" {
		t.Errorf("CORSOrigins = %#v", c.CORSOrigins)
	}
}

func TestLoadConfigInvalidModeDoesNotFallBackSilently(t *testing.T) {
	t.Setenv("ORCATERM_MODE", "bogus")
	c := loadConfig()
	if c.Mode != "bogus" {
		t.Fatalf("Mode = %q，非法值不应被静默替换", c.Mode)
	}
	if err := validateConfig(c); err == nil {
		t.Fatal("非法 Mode 必须在启动前失败")
	}
}
