package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// runExecute drives the REAL command entrypoint — cli.Execute, the same
// function cmd/tellury/main.go calls — against the given command-line
// arguments, capturing the actual stdout and stderr file descriptors. It
// returns the exit code, the error Execute returned, and the captured output.
//
// Execute reads os.Args and writes to os.Stdout/os.Stderr, so the helper
// swaps those out for pipes for the duration of the call. This is what lets a
// test assert the operator-visible contract ("exit code 2, nothing on
// stdout") rather than an in-process approximation of it.
func runExecute(t *testing.T, args ...string) (code int, execErr error, stdout, stderr string) {
	t.Helper()

	oldArgs := os.Args
	oldStdout, oldStderr := os.Stdout, os.Stderr
	defer func() { os.Args, os.Stdout, os.Stderr = oldArgs, oldStdout, oldStderr }()

	os.Args = append([]string{"tellury"}, args...)

	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(stdout): %v", err)
	}
	os.Stdout = wOut
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(stderr): %v", err)
	}
	os.Stderr = wErr

	code, execErr = Execute()

	wOut.Close()
	wErr.Close()
	out, _ := io.ReadAll(rOut)
	errOut, _ := io.ReadAll(rErr)
	return code, execErr, string(out), string(errOut)
}

// TestExecute_TwoProviderConflictExitsUsage is the acceptance test for the
// two-provider failure, driven through the real command surface:
//
//	tellury scan --gcp-project my-project --aws-account 123456789012
//
// must fail BEFORE any work, with exit code 2 (the usage-error code), nothing
// on stdout, and an error naming both providers' flag groups and telling the
// operator to pick one.
func TestExecute_TwoProviderConflictExitsUsage(t *testing.T) {
	code, execErr, stdout, _ := runExecute(t,
		"scan", "--gcp-project", "my-project", "--aws-account", "123456789012")

	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d (usage error)", code, ExitUsage)
	}
	if execErr == nil {
		t.Fatal("Execute must return an error for a two-provider scope conflict")
	}
	var ue usageError
	if !errors.As(execErr, &ue) {
		t.Fatalf("error type = %T, want a usage error (the type Execute maps to exit %d): %v",
			execErr, ExitUsage, execErr)
	}
	if stdout != "" {
		t.Errorf("a two-provider conflict must write nothing to stdout; got:\n%s", stdout)
	}

	msg := execErr.Error()
	for _, want := range []string{
		"both",
		"GCP",
		"AWS",
		"--gcp-project",
		"--gcp-folder",
		"--gcp-organization",
		"--aws-account",
		"--aws-organizational-unit",
		"--aws-organization",
		"pick one provider",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict error must name %q; got: %s", want, msg)
		}
	}
}

// TestExecute_ProviderConflictsCoverEveryPair extends the two-provider
// conflict from gcp+aws to every pair among gcp, aws and azure. A pair of
// scope flags must remain a usage error, and the message must name the two
// providers that actually collided — never a generic "multiple providers"
// that leaves the operator guessing which flags to remove.
func TestExecute_ProviderConflictsCoverEveryPair(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		first string
		other string
		flags []string
	}{
		{
			name:  "gcp and aws",
			args:  []string{"--gcp-project", "my-project", "--aws-account", "123456789012"},
			first: "GCP",
			other: "AWS",
			flags: []string{
				"--gcp-project", "--gcp-folder", "--gcp-organization",
				"--aws-account", "--aws-organizational-unit", "--aws-organization",
			},
		},
		{
			name:  "gcp and azure",
			args:  []string{"--gcp-project", "my-project", "--azure-subscription", "22222222-2222-2222-2222-222222222222"},
			first: "GCP",
			other: "AZURE",
			flags: []string{
				"--gcp-project", "--gcp-folder", "--gcp-organization",
				"--azure-management-group", "--azure-resource-group", "--azure-subscription", "--azure-tenant",
			},
		},
		{
			name:  "aws and azure",
			args:  []string{"--aws-account", "123456789012", "--azure-subscription", "22222222-2222-2222-2222-222222222222"},
			first: "AWS",
			other: "AZURE",
			flags: []string{
				"--aws-account", "--aws-organizational-unit", "--aws-organization",
				"--azure-management-group", "--azure-resource-group", "--azure-subscription", "--azure-tenant",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, execErr, stdout, _ := runExecute(t, append([]string{"scan"}, tc.args...)...)

			if code != ExitUsage {
				t.Fatalf("exit code = %d, want %d (usage error)", code, ExitUsage)
			}
			if execErr == nil {
				t.Fatal("Execute must return an error for a provider scope conflict")
			}
			var ue usageError
			if !errors.As(execErr, &ue) {
				t.Fatalf("error type = %T, want a usage error: %v", execErr, execErr)
			}
			if stdout != "" {
				t.Errorf("a provider conflict must write nothing to stdout; got:\n%s", stdout)
			}

			msg := execErr.Error()
			for _, want := range append([]string{"both", tc.first, tc.other, "pick one provider"}, tc.flags...) {
				if !strings.Contains(msg, want) {
					t.Errorf("conflict error must name %q; got: %s", want, msg)
				}
			}
		})
	}
}

// TestExecute_AWSAccountAloneSelectsAWS is the end-to-end proof of provider
// inference AND of AWS rule registration: `tellury scan --aws-account
// 123456789012` (no --provider) must pass validation — inferring the AWS
// provider — and proceed past rule selection, because this build now ships
// the two AWS native rules (unattached_ebs_volume, unassociated_eip). The
// scan then reaches live AWS provider construction and ingestion; with no AWS
// credentials on the host (the CI/sandbox case) that fails as an OPERATIONAL
// error (exit 1) naming the AWS provider. It must NEVER be the old
// "no rules selected for provider \"aws\"" usage error (that would mean the
// AWS rules silently stopped registering), never a two-provider conflict and
// never a GCP-shaped scope error.
func TestExecute_AWSAccountAloneSelectsAWS(t *testing.T) {
	// The credential chain's last resort is the EC2 instance metadata service
	// at 169.254.169.254. Off an EC2 instance that probe is a real network
	// call, which made this test take five seconds and would make it flaky in
	// any CI environment that blackholes the address rather than refusing it.
	// The SDK honours this variable by skipping IMDS entirely, so the test
	// exercises the same no-credentials path with no network at all.
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	// This test asserts the NO-CREDENTIALS path, so it must not inherit any
	// the developer happens to have. Without this it passes on CI and fails on
	// a machine with a working AWS profile — the scan succeeds there and
	// returns a different exit code. Clearing them makes the test say the same
	// thing everywhere.
	for _, v := range []string{
		"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
	} {
		t.Setenv(v, "")
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))

	code, execErr, stdout, _ := runExecute(t, "scan", "--aws-account", "123456789012")

	if code != ExitError {
		t.Fatalf("exit code = %d, want %d (operational: a live AWS scan with no credentials fails at ingestion, not at rule selection)",
			code, ExitError)
	}
	if execErr == nil {
		t.Fatal("Execute must return an error")
	}
	msg := execErr.Error()
	if strings.Contains(msg, `no rules selected for provider "aws"`) {
		t.Fatalf("AWS rules must be registered so a --aws-account scan reaches ingestion; got the no-AWS-rules usage error: %v", execErr)
	}
	if strings.Contains(msg, "both") || strings.Contains(msg, "unknown provider") {
		t.Fatalf("a --aws-account scan must not be treated as a conflict or an unknown provider; got: %v", execErr)
	}
	if !strings.Contains(msg, "aws") {
		t.Fatalf("a --aws-account scan must reach the AWS provider and fail there (no credentials); got: %v", execErr)
	}
	if stdout != "" {
		t.Errorf("an AWS operational failure must write nothing to stdout; got:\n%s", stdout)
	}
}

// TestExecute_GCPProjectAloneStillWorks pins that the GCP default path is
// untouched by inference: `tellury scan --gcp-project p` resolves to the GCP
// provider — no scope env var needed, no --provider — and the scan proceeds
// past validation and rule selection into live GCP provider construction.
// With no Application Default Credentials on the host (the CI/sandbox case)
// that construction fails as an OPERATIONAL error (exit 1), naming the GCP
// provider — never a two-provider conflict, never an unknown-provider error,
// never a usage error at all. A regression in inference would instead surface
// as exit 2 with "both ... scope flags" or "unknown provider".
func TestExecute_GCPProjectAloneStillWorks(t *testing.T) {
	code, execErr, _, _ := runExecute(t, "scan", "--gcp-project", "my-project")

	if code != ExitError {
		t.Fatalf("exit code = %d, want %d (operational: a live GCP scan with no ADC fails at provider construction, not at usage)",
			code, ExitError)
	}
	if execErr == nil {
		t.Fatal("Execute must return an error")
	}
	msg := execErr.Error()
	if strings.Contains(msg, "both") || strings.Contains(msg, "unknown provider") {
		t.Fatalf("a --gcp-project scan must not be treated as a conflict or an unknown provider; got: %v", execErr)
	}
	if !strings.Contains(msg, "gcp:") {
		t.Fatalf("a --gcp-project scan must reach the GCP provider and fail there (no ADC); got: %v", execErr)
	}
}

// TestScanAndGraphExportExposeAllProviderFlagSets asserts that GCP's, AWS's
// and Azure's scope flags all appear on `tellury scan` and `tellury graph
// export` — the requirement that the CLI surface is registry-driven and
// multi-cloud, not GCP-only.
func TestScanAndGraphExportExposeAllProviderFlagSets(t *testing.T) {
	commands := []struct {
		name string
		cmd  interface {
			Flags() *pflag.FlagSet
		}
	}{
		{"scan", newScanCmd(&globalFlags{})},
		{"graph export", newGraphExportCmd(&globalFlags{})},
	}
	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			fs := tc.cmd.Flags()
			for _, name := range []string{
				"gcp-project", "gcp-folder", "gcp-organization",
				"aws-account", "aws-organizational-unit", "aws-organization",
				"azure-tenant", "azure-management-group", "azure-subscription", "azure-resource-group",
			} {
				if fs.Lookup(name) == nil {
					t.Errorf("%s must expose --%s", tc.name, name)
				}
			}
			// The --provider flag must default to empty so the scope flags can
			// infer the provider (the historical "gcp" default would make
			// --aws-account or --azure-subscription alone a provider conflict).
			fl := fs.Lookup("provider")
			if fl == nil {
				t.Fatalf("%s must expose --provider", tc.name)
			}
			if fl.DefValue != "" {
				t.Errorf("%s --provider default = %q, want \"\" so scope flags infer the provider",
					tc.name, fl.DefValue)
			}
		})
	}
}
