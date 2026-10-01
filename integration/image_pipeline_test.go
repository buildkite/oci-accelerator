package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Git uses real local history. Only Docker and the secret service are replaced.
// These tests must never contact Docker Hub or retrieve a real credential.
func TestImagePipeline(t *testing.T) {
	requireCommands(t, "bash", "git")
	script, err := filepath.Abs("../.buildkite/image.sh")
	if err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := t.TempDir()
	git(origin, "init", "-b", "main")
	git(origin, "commit", "--allow-empty", "-m", "older main")
	git(origin, "branch", "older")
	git(origin, "commit", "--allow-empty", "-m", "later main")
	git(origin, "checkout", "-b", "unmerged")
	git(origin, "commit", "--allow-empty", "-m", "unmerged")
	git(origin, "checkout", "main")

	for _, tc := range []struct {
		name, mode, branch, tag, checkout, pr, failure string
		wantError, wantSecret, wantBuild, wantPush     bool
	}{
		{name: "check without credentials", mode: "check", wantBuild: true},
		{name: "publish build number and edge from shallow main", wantSecret: true, wantBuild: true, wantPush: true},
		{name: "feature branch", branch: "feature", wantError: true},
		{name: "tag build", tag: "v1.2.3", wantError: true},
		{name: "pull request", pr: "16", wantError: true},
		{name: "missing build number", failure: "number-missing", wantError: true},
		{name: "invalid build number", failure: "number-invalid", wantError: true},
		{name: "outdated queued build", checkout: "older"},
		{name: "unmerged commit claiming main", checkout: "unmerged"},
		{name: "mismatched build commit", failure: "commit", wantError: true},
		{name: "fetch failure", failure: "fetch", wantError: true},
		{name: "untracked source", failure: "untracked", wantError: true},
		{name: "ignored source", failure: "ignored", wantError: true},
		{name: "login failure", failure: "login", wantError: true, wantSecret: true},
		{name: "secret failure", failure: "secret", wantError: true, wantSecret: true},
		{name: "build failure", failure: "build", wantError: true, wantSecret: true, wantBuild: true, wantPush: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			checkout := tc.checkout
			if checkout == "" {
				checkout = "main"
			}
			git(root, "clone", "--depth", "1", "--branch", checkout, "file://"+origin, "repo")
			repo := filepath.Join(root, "repo")
			if tc.failure == "fetch" {
				git(repo, "remote", "set-url", "origin", filepath.Join(root, "missing"))
			}
			if tc.failure == "untracked" || tc.failure == "ignored" {
				if err := os.Mkdir(filepath.Join(repo, "internal"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(repo, "internal", "injected.go"), "package injected\n", 0o600)
				if tc.failure == "ignored" {
					writeFile(t, filepath.Join(repo, ".git", "info", "exclude"), "internal/injected.go\n", 0o600)
				}
			}
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$IMAGE_TEST_ROOT/docker-calls"
printf '%s\n' "$DOCKER_CONFIG" > "$IMAGE_TEST_ROOT/docker-config"
case "$1 $2" in
  'login --username')
    [[ "$*" == 'login --username buildkite --password-stdin docker.io' ]]
    [[ $(cat) == fixture-token ]]
    [[ "$IMAGE_TEST_FAILURE" != login ]]
    ;;
  'buildx build') [[ "$IMAGE_TEST_FAILURE" != build ]] ;;
  'buildx create'|'buildx rm') ;;
  *) exit 99 ;;
esac
`, 0o700)
			writeFile(t, filepath.Join(bin, "buildkite-agent"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$IMAGE_TEST_ROOT/secret-calls"
[[ "$*" == 'secret get --format default OCI_ACCELERATOR_DOCKER_HUB_TOKEN' ]]
[[ "$IMAGE_TEST_FAILURE" != secret ]]
printf fixture-token
`, 0o700)
			mode := tc.mode
			if mode == "" {
				mode = "publish"
			}
			pr := tc.pr
			if pr == "" {
				pr = "false"
			}
			branch := tc.branch
			if branch == "" {
				branch = "main"
			}
			commit := git(repo, "rev-parse", "HEAD")
			if tc.failure == "commit" {
				commit = strings.Repeat("0", 40)
			}
			number := "108"
			if mode == "check" || tc.failure == "number-missing" {
				number = ""
			} else if tc.failure == "number-invalid" {
				number = "108/invalid"
			}
			cmd := exec.Command("bash", script, mode)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
				"BUILDKITE_BRANCH="+branch, "BUILDKITE_TAG="+tc.tag, "BUILDKITE_PULL_REQUEST="+pr, "BUILDKITE_COMMIT="+commit,
				"BUILDKITE_BUILD_NUMBER="+number, "IMAGE_TEST_ROOT="+root, "IMAGE_TEST_FAILURE="+tc.failure)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v: %s", err, tc.wantError, out)
			}
			if strings.Contains(string(out), "fixture-token") {
				t.Fatal("credential appeared in output")
			}
			_, err = os.Stat(filepath.Join(root, "secret-calls"))
			if (err == nil) != tc.wantSecret {
				t.Fatalf("secret retrieved = %v, want %v", err == nil, tc.wantSecret)
			}
			calls, _ := os.ReadFile(filepath.Join(root, "docker-calls"))
			if strings.Contains(string(calls), "buildx build ") != tc.wantBuild || strings.Contains(string(calls), "--push") != tc.wantPush {
				t.Fatalf("unexpected Docker operations: %s", calls)
			}
			if tc.wantBuild && !strings.Contains(string(calls), "--platform linux/amd64,linux/arm64") {
				t.Fatalf("missing platform pair: %s", calls)
			}
			if tc.wantPush && (!strings.Contains(string(calls), "--tag docker.io/buildkite/oci-accelerator:build-108 --tag docker.io/buildkite/oci-accelerator:edge --push") || strings.Count(string(calls), "--tag ") != 2) {
				t.Fatalf("expected the build number and edge tags: %s", calls)
			}
			if config, err := os.ReadFile(filepath.Join(root, "docker-config")); err == nil {
				if _, err := os.Stat(strings.TrimSpace(string(config))); !os.IsNotExist(err) {
					t.Fatalf("temporary Docker credentials not cleaned: %v", err)
				}
			}
		})
	}
}
