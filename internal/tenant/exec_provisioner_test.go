package tenant

import (
	"context"
	"runtime"
	"testing"
)

func TestParseProvisionOutput(t *testing.T) {
	t.Parallel()
	out := []byte(`{"name":"shard-auto-1","stalwart_url":"https://s1:8080","postgres_dsn":"postgres://x","max_mailboxes":5000}`)
	sh, err := parseProvisionOutput(out, "fallback")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sh.Name != "shard-auto-1" || sh.StalwartURL != "https://s1:8080" || sh.MaxMailboxes != 5000 {
		t.Fatalf("unexpected shard: %+v", sh)
	}
}

func TestParseProvisionOutputDefaultsName(t *testing.T) {
	t.Parallel()
	sh, err := parseProvisionOutput([]byte(`{"stalwart_url":"https://s2:8080"}`), "fallback-name")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sh.Name != "fallback-name" {
		t.Fatalf("name not defaulted: %+v", sh)
	}
}

func TestParseProvisionOutputErrors(t *testing.T) {
	t.Parallel()
	if _, err := parseProvisionOutput([]byte(""), "n"); err == nil {
		t.Fatal("empty output should error")
	}
	if _, err := parseProvisionOutput([]byte("not json"), "n"); err == nil {
		t.Fatal("bad json should error")
	}
	if _, err := parseProvisionOutput([]byte(`{"name":"x"}`), "n"); err == nil {
		t.Fatal("missing stalwart_url should error")
	}
}

func TestExecShardProvisionerRunsCommand(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh")
	}
	// A real command whose stdout satisfies the JSON contract.
	p := &ExecShardProvisioner{
		Command: "sh",
		Args:    []string{"-c", `printf '{"stalwart_url":"https://provisioned:8080"}'; :`},
	}
	// The trailing positional `name` arg is harmless to `sh -c` (it
	// becomes $0), and parseProvisionOutput defaults the name from it.
	sh, err := p.Provision(context.Background(), "shard-auto-9")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if sh.StalwartURL != "https://provisioned:8080" || sh.Name != "shard-auto-9" {
		t.Fatalf("unexpected shard: %+v", sh)
	}
}

func TestExecShardProvisionerNoCommand(t *testing.T) {
	t.Parallel()
	p := &ExecShardProvisioner{}
	if _, err := p.Provision(context.Background(), "x"); err == nil {
		t.Fatal("expected error when no command configured")
	}
}
