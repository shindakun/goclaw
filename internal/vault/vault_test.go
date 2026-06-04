package vault

import "testing"

func TestReachable(t *testing.T) {
	if (Config{}).Reachable() {
		t.Fatal("empty Config should be unreachable")
	}
	if !(Config{ProxyURL: "http://host:18080"}).Reachable() {
		t.Fatal("Config with ProxyURL should be reachable")
	}
}

func TestEnv(t *testing.T) {
	t.Run("no proxy yields nil", func(t *testing.T) {
		if env := (Config{}).Env(); env != nil {
			t.Fatalf("expected nil env without ProxyURL, got %v", env)
		}
	})

	t.Run("proxy populates HTTP/HTTPS/NO_PROXY", func(t *testing.T) {
		c := Config{ProxyURL: "http://host:18080", NoProxy: "localhost,127.0.0.1"}
		env := c.Env()
		if env["HTTP_PROXY"] != c.ProxyURL || env["HTTPS_PROXY"] != c.ProxyURL {
			t.Fatalf("proxy env not set: %v", env)
		}
		if env["NO_PROXY"] != "localhost,127.0.0.1" {
			t.Fatalf("NO_PROXY = %q", env["NO_PROXY"])
		}
	})
}

func TestCAMount(t *testing.T) {
	t.Run("no CA paths yields no mount", func(t *testing.T) {
		if _, ok := (Config{}).CAMount(); ok {
			t.Fatal("expected no mount without CA paths")
		}
		// Partial config (only host path) also yields nothing.
		if _, ok := (Config{CAHostPath: "/x/ca.pem"}).CAMount(); ok {
			t.Fatal("expected no mount with only host path")
		}
		if _, ok := (Config{CAContainerPath: "/etc/ca.pem"}).CAMount(); ok {
			t.Fatal("expected no mount with only container path")
		}
	})

	t.Run("both CA paths yield a read-only mount", func(t *testing.T) {
		c := Config{CAHostPath: "/host/ca.pem", CAContainerPath: "/etc/ssl/ca.pem"}
		m, ok := c.CAMount()
		if !ok {
			t.Fatal("expected a mount when both CA paths set")
		}
		if m.HostPath != c.CAHostPath || m.ContainerPath != c.CAContainerPath {
			t.Fatalf("mount paths wrong: %+v", m)
		}
		if m.ReadWrite {
			t.Fatal("CA mount must be read-only")
		}
	})
}
