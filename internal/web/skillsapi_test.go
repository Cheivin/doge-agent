package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillsEndpoints: /api/skills lists the installed skills (fresh scan of
// the discovery dirs) with metadata only; /api/skills/{name} returns the body
// plus the bundle file tree; unknown names 404. The session view comes from
// the backend's loaded set.
func TestSkillsEndpoints(t *testing.T) {
	// Point the global agent dir at a temp dir so the scan is hermetic.
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	os.MkdirAll(filepath.Join(dir, "skills", "demo"), 0o755)
	os.WriteFile(filepath.Join(dir, "skills", "demo", "SKILL.md"),
		[]byte("---\nname: demo\ndescription: A demo skill\n---\n# Demo\nDo the thing.\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "skills", "demo", "references"), 0o755)
	os.WriteFile(filepath.Join(dir, "skills", "demo", "references", "guide.md"),
		[]byte("reference body"), 0o644)

	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	// 未认证 401。
	anon := &http.Client{}
	if resp, err := anon.Get(ts.URL + "/api/skills"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anon status = %d, want 401", resp.StatusCode)
		}
	}

	m := getJSON(t, c, ts.URL+"/api/skills", http.StatusOK)
	skills, _ := m["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("skills = %#v", m["skills"])
	}
	first, _ := skills[0].(map[string]any)
	if first["name"] != "demo" || first["description"] != "A demo skill" {
		t.Fatalf("skill summary = %#v", first)
	}
	if first["content"] != nil {
		t.Fatalf("list must not carry the body: %#v", first)
	}
	dirs, _ := m["dirs"].([]any)
	if len(dirs) != 2 {
		t.Fatalf("dirs = %#v", m["dirs"])
	}

	// 详情：正文 + 文件树。
	d := getJSON(t, c, ts.URL+"/api/skills/demo", http.StatusOK)
	if d["name"] != "demo" {
		t.Fatalf("detail = %#v", d)
	}
	if !strings.Contains(d["content"].(string), "Do the thing.") {
		t.Fatalf("detail content = %#v", d["content"])
	}
	files, _ := d["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("detail files = %#v (want SKILL.md + references/guide.md)", d["files"])
	}

	// 未知技能 404。
	getJSON(t, c, ts.URL+"/api/skills/nope", http.StatusNotFound)
}

// TestSessionSkillsEndpoint: /api/session/skills reports the backend's loaded
// skill set (the set behind the session's skill tool).
func TestSessionSkillsEndpoint(t *testing.T) {
	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	m := getJSON(t, c, ts.URL+"/api/session/skills", http.StatusOK)
	skills, _ := m["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("session skills = %#v", m["skills"])
	}
	first, _ := skills[0].(map[string]any)
	if first["name"] != "demo" {
		t.Fatalf("session skill = %#v", first)
	}
}

// TestConfigSkillsOverview: /api/config carries the skills section (dirs +
// names) even before any skill panel is opened.
func TestConfigSkillsOverview(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOGE_CODING_AGENT_DIR", dir)
	os.MkdirAll(filepath.Join(dir, "skills", "demo"), 0o755)
	os.WriteFile(filepath.Join(dir, "skills", "demo", "SKILL.md"),
		[]byte("---\nname: demo\ndescription: A demo skill\n---\n# Demo\nDo the thing.\n"), 0o644)

	f := &fakeBackend{}
	_, ts := newTestServer(t, f)
	c := bearerClient("tok-123")

	m := getJSON(t, c, ts.URL+"/api/config", http.StatusOK)
	skills, _ := m["skills"].(map[string]any)
	if skills == nil || skills["count"].(float64) != 1 {
		t.Fatalf("config skills section = %#v", m["skills"])
	}
	names, _ := skills["names"].([]any)
	if len(names) != 1 || names[0] != "demo" {
		t.Fatalf("config skills names = %#v", skills["names"])
	}
}
