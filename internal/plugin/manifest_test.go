package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadManifest_Valid(t *testing.T) {
	dir := writeManifest(t, "name: roll\nkind: tool\nversion: \"1.0.0\"\nexec: roll\ncommand: roll\n")
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if m.Name != "roll" || m.Exec != "roll" || m.ExecPath() != filepath.Join(dir, "roll") {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

func TestLoadManifest_RejectsBadExec(t *testing.T) {
	cases := map[string]string{
		"path traversal": "name: x\nkind: tool\nexec: ../evil\n",
		"absolute path":  "name: x\nkind: tool\nexec: /bin/sh\n",
		"space in name":  "name: x\nkind: tool\nexec: my plugin\n",
		"missing exec":   "name: x\nkind: tool\n",
		"unknown kind":   "name: x\nkind: gadget\nexec: x\n",
		"missing name":   "kind: tool\nexec: x\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadManifest(writeManifest(t, body)); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
		})
	}
}
