package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getJSON(t *testing.T, ps *ProxyServer, path string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	ps.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	var body map[string]interface{}
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func modelIDsOf(t *testing.T, body map[string]interface{}) map[string]map[string]interface{} {
	t.Helper()
	data, ok := body["data"].([]interface{})
	if !ok {
		t.Fatalf("模型列表缺少 data 数组: %v", body)
	}
	out := make(map[string]map[string]interface{}, len(data))
	for _, raw := range data {
		entry, _ := raw.(map[string]interface{})
		id, _ := entry["id"].(string)
		out[id] = entry
	}
	return out
}

// 目录必须是 8 current + 9 legacy，加上 1 个 deprecated 别名。
func TestModelsListCounts(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", nil)
	status, body := getJSON(t, ps, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("状态 %d", status)
	}
	entries := modelIDsOf(t, body)
	if len(entries) != 18 {
		t.Errorf("列表项 = %d，期望 18（17 唯一模型 + 1 别名）", len(entries))
	}
	current, legacy := 0, 0
	for _, m := range Models() {
		if m.Legacy {
			legacy++
		} else {
			current++
		}
	}
	if current != 8 || legacy != 9 || len(Models()) != 17 {
		t.Errorf("目录 = %d current / %d legacy（共 %d），期望 8 / 9 / 17", current, legacy, len(Models()))
	}
	if _, ok := entries[DefaultModelID]; !ok {
		t.Errorf("默认模型 %s 应出现在列表中", DefaultModelID)
	}
}

// 别名必须在 list 与 retrieve 中完全一致（同样带 alias_of 与 deprecated）。
func TestModelAliasListRetrieveConsistency(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", nil)
	def := ps.defaultModel()

	_, list := getJSON(t, ps, "/v1/models")
	entries := modelIDsOf(t, list)
	aliasEntry, ok := entries[LegacyModelAlias]
	if !ok {
		t.Fatalf("列表缺少别名 %s: %v", LegacyModelAlias, entries)
	}

	status, retrieve := getJSON(t, ps, "/v1/models/"+LegacyModelAlias)
	if status != http.StatusOK {
		t.Fatalf("别名 retrieve 状态 %d: %v", status, retrieve)
	}
	for _, key := range []string{"id", "alias_of", "upstream", "name", "deprecated", "object"} {
		if retrieve[key] != aliasEntry[key] {
			t.Errorf("%s: retrieve=%v list=%v", key, retrieve[key], aliasEntry[key])
		}
	}
	if retrieve["alias_of"] != def.ID {
		t.Errorf("alias_of = %v，期望 %v", retrieve["alias_of"], def.ID)
	}
	if retrieve["deprecated"] != true {
		t.Errorf("别名应标记 deprecated: %v", retrieve["deprecated"])
	}
	// 别名不是第 18 个上游模型
	if _, dup := modelIDsOf(t, map[string]interface{}{"data": []interface{}{retrieve}})[def.ID]; dup {
		t.Errorf("别名不应与默认模型 id 相同")
	}
}

func TestModelRetrieveKnownAndUnknown(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", nil)

	status, body := getJSON(t, ps, "/v1/models/glm-5.3")
	if status != http.StatusOK || body["id"] != "glm-5.3" {
		t.Fatalf("已知模型 retrieve 失败: %d %v", status, body)
	}
	if body["upstream"] != "TokenHub/glm-5.3" {
		t.Errorf("upstream = %v", body["upstream"])
	}
	if _, hasAlias := body["alias_of"]; hasAlias {
		t.Errorf("普通模型不应带 alias_of: %v", body)
	}

	status, body = getJSON(t, ps, "/v1/models/definitely-not-a-model")
	if status != http.StatusNotFound {
		t.Fatalf("未知模型应 404，实际 %d %v", status, body)
	}
	errObj, _ := body["error"].(map[string]interface{})
	if errObj["code"] != "model_not_found" {
		t.Errorf("错误码 = %v", errObj["code"])
	}
}

// 别名等于默认模型 id 时不应产生独立列表项，retrieve 也回落到普通条目。
func TestModelAliasCollapsesToDefault(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", func(c *Config) { c.AliasModel = DefaultModelID })

	_, list := getJSON(t, ps, "/v1/models")
	entries := modelIDsOf(t, list)
	if len(entries) != 17 {
		t.Errorf("别名与默认模型重合时列表应为 17 项，实际 %d", len(entries))
	}
	status, body := getJSON(t, ps, "/v1/models/"+DefaultModelID)
	if status != http.StatusOK {
		t.Fatalf("状态 %d", status)
	}
	if _, hasAlias := body["alias_of"]; hasAlias {
		t.Errorf("重合时不应带 alias_of: %v", body)
	}
	if body["default"] != true {
		t.Errorf("默认模型应标记 default: %v", body)
	}
}

func TestModelsMethodNotAllowed(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	ps.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("状态 %d，期望 405", rec.Code)
	}
}
