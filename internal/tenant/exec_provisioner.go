package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ExecShardProvisioner is a ShardProvisioner that shells out to an
// operator-supplied command (typically `scripts/provision-shard.sh`,
// which wraps `terraform apply` against the `deploy/terraform/shard/`
// module). The command receives the desired shard name as its single
// argument and must print a JSON object describing the resulting
// shard to stdout:
//
//	{"name":"shard-auto-123","stalwart_url":"https://...","postgres_dsn":"...","max_mailboxes":10000}
//
// Decoupling provisioning behind a command keeps the Go side free of a
// hard Terraform/Pulumi dependency and lets the same binary drive any
// IaC tool (or a cloud API) by swapping the script. The JSON contract
// is the seam that is unit-tested here; the script itself is covered
// by the deploy/ module's own plan/apply checks.
type ExecShardProvisioner struct {
	// Command is the executable to run (argv[0]). Required.
	Command string
	// Args are fixed arguments prepended before the shard name.
	Args []string
	// Timeout bounds a single provision run. Defaults to 15m — a
	// real `terraform apply` standing up VMs takes minutes.
	Timeout time.Duration
}

// provisionResult is the JSON contract the provisioning command emits.
type provisionResult struct {
	Name         string `json:"name"`
	StalwartURL  string `json:"stalwart_url"`
	PostgresDSN  string `json:"postgres_dsn"`
	MaxMailboxes int    `json:"max_mailboxes"`
}

// Provision runs the configured command and parses its stdout into a
// Shard. Implements ShardProvisioner.
func (p *ExecShardProvisioner) Provision(ctx context.Context, name string) (Shard, error) {
	if p.Command == "" {
		return Shard{}, fmt.Errorf("exec provisioner: no command configured")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append(append([]string{}, p.Args...), name)
	cmd := exec.CommandContext(runCtx, p.Command, args...)
	out, err := cmd.Output()
	if err != nil {
		return Shard{}, fmt.Errorf("exec provisioner: run %s: %w", p.Command, err)
	}
	return parseProvisionOutput(out, name)
}

// parseProvisionOutput decodes the command's stdout. Split out from
// Provision so the JSON contract is testable without spawning a
// process.
func parseProvisionOutput(out []byte, name string) (Shard, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return Shard{}, fmt.Errorf("exec provisioner: command produced no output")
	}
	var res provisionResult
	if err := json.Unmarshal([]byte(trimmed), &res); err != nil {
		return Shard{}, fmt.Errorf("exec provisioner: parse output: %w", err)
	}
	if res.StalwartURL == "" {
		return Shard{}, fmt.Errorf("exec provisioner: command output missing stalwart_url")
	}
	if res.Name == "" {
		res.Name = name
	}
	return Shard{
		Name:         res.Name,
		StalwartURL:  res.StalwartURL,
		PostgresDSN:  res.PostgresDSN,
		MaxMailboxes: res.MaxMailboxes,
	}, nil
}
