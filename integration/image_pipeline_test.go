package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Git uses real local history. Docker, Buildkite, and GitHub are replaced.
// These tests must never contact Docker Hub or retrieve a real credential.
func TestImagePipeline(t *testing.T) {
	requireCommands(t, "bash", "git", "jq", "sha256sum")

	script, err := filepath.Abs("../.buildkite/image.sh")
	if err != nil {
		t.Fatal(err)
	}

	origin := imagePipelineHistory(t)

	for _, tc := range []struct {
		name, mode, branch, tag, checkout, pr, failure string
		wantError, wantSecret, wantBuild, wantPush     bool
		version                                        string
		wantInspect                                    bool
		wantTags                                       []string
		annotationVersion, annotationHeading           string
	}{
		{name: "check without credentials", mode: "check", wantBuild: true},
		{name: "publish build number and latest from shallow main", wantSecret: true, wantBuild: true, wantPush: true, wantTags: []string{"build-108", "latest"}},
		{name: "publish first release", checkout: "first-release", wantSecret: true, wantBuild: true, wantPush: true, wantInspect: true, wantTags: []string{"v0.1.0"}, annotationVersion: "v0.1.0", annotationHeading: "**Published oci-accelerator v0.1.0**"},
		{name: "merged release after main advances", checkout: "release", wantSecret: true, wantBuild: true, wantPush: true, wantInspect: true, wantTags: []string{"v0.1.10"}, annotationVersion: "v0.1.10", annotationHeading: "**Published oci-accelerator v0.1.10**"},
		{name: "release at main tip", failure: "main-tip", wantSecret: true, wantBuild: true, wantPush: true, wantInspect: true, wantTags: []string{"v0.2.0", "build-108", "latest"}, annotationVersion: "v0.2.0", annotationHeading: "**Published oci-accelerator v0.2.0**"},
		{name: "retry already published release", checkout: "release", failure: "published", wantSecret: true, wantInspect: true, annotationVersion: "v0.1.10", annotationHeading: "**Version v0.1.10 is already published**"},
		{name: "retry published release at tip", failure: "published-tip", wantSecret: true, wantBuild: true, wantPush: true, wantInspect: true, wantTags: []string{"build-108", "latest"}, annotationVersion: "v0.2.0", annotationHeading: "**Version v0.2.0 is already published**"},
		{name: "conflicting version tag", checkout: "release", failure: "conflict", wantSecret: true, wantError: true, wantInspect: true},
		{name: "tag without metadata", checkout: "release", failure: "no-metadata", wantSecret: true, wantError: true, wantInspect: true},
		{name: "registry unauthorized", checkout: "release", failure: "unauthorized", wantSecret: true, wantError: true, wantInspect: true},
		{name: "registry timeout", checkout: "release", failure: "timeout", wantSecret: true, wantError: true, wantInspect: true},
		{name: "registry unexpected response", checkout: "release", failure: "unknown", wantSecret: true, wantError: true, wantInspect: true},
		{name: "PR version bump", checkout: "unmerged", branch: "feature", pr: "16", mode: "check", version: "v0.2.0\n", wantBuild: true},
		{name: "initial prefixed version", checkout: "before-version", branch: "feature", pr: "16", mode: "check", version: "v0.1.0\n", wantBuild: true},
		{name: "PR downgrade", checkout: "unmerged", branch: "feature", pr: "16", mode: "check", version: "v0.1.9\n", wantError: true},
		{name: "prefix-only change rejected", checkout: "unprefixed", version: "v0.1.0\n", wantError: true},
		{name: "unprefixed comparison rejected", checkout: "unprefixed", version: "v0.1.1\n", wantError: true},
		{name: "PR with earlier bump then unchanged tip", checkout: "unmerged", branch: "feature", pr: "16", mode: "check", version: "v0.1.9\n", failure: "extra-commit", wantError: true},
		{name: "missing version prefix", mode: "check", version: "0.2.0\n", wantError: true},
		{name: "leading zero version", mode: "check", version: "v0.02.0\n", wantError: true},
		{name: "prerelease version", mode: "check", version: "v0.2.0-rc.1\n", wantError: true},
		{name: "multiple version lines", mode: "check", version: "v0.2.0\nv0.3.0\n", wantError: true},
		{name: "trailing blank line", mode: "check", version: "v0.2.0\n\n", wantError: true},
		{name: "binary version", mode: "check", version: "v0.2.0\x00\n", wantError: true},
		{name: "version without trailing newline", mode: "check", version: "v0.2.0", wantBuild: true},
		{name: "missing version", mode: "check", failure: "missing-version", wantError: true},
		{name: "maximum length tag", mode: "check", version: "v" + strings.Repeat("1", 123) + ".0.0\n", wantBuild: true},
		{name: "oversize tag", mode: "check", version: "v" + strings.Repeat("1", 124) + ".0.0\n", wantError: true},
		{name: "feature branch", branch: "feature", wantError: true},
		{name: "tag build", tag: "v1.2.3", wantError: true},
		{name: "pull request", pr: "16", wantError: true},
		{name: "missing build number", failure: "number-missing", wantError: true},
		{name: "invalid build number", failure: "number-invalid", wantError: true},
		{name: "outdated queued build", checkout: "older"},
		{name: "unmerged commit claiming main", checkout: "unmerged", wantError: true},
		{name: "merged side commit claiming main", checkout: "release-pr", wantError: true},
		{name: "mismatched build commit", failure: "commit", wantError: true},
		{name: "fetch failure", failure: "fetch", wantError: true},
		{name: "untracked source", failure: "untracked", wantError: true},
		{name: "ignored source", failure: "ignored", wantError: true},
		{name: "login failure", failure: "login", wantError: true, wantSecret: true},
		{name: "secret failure", failure: "secret", wantError: true, wantSecret: true},
		{name: "build failure", failure: "build", wantError: true, wantSecret: true, wantBuild: true, wantPush: true, wantTags: []string{"build-108", "latest"}},
		{name: "release build failure", checkout: "release", failure: "build", wantError: true, wantSecret: true, wantBuild: true, wantPush: true, wantInspect: true, wantTags: []string{"v0.1.10"}},
		{name: "annotation failure is nonfatal", checkout: "release", failure: "annotation", wantSecret: true, wantBuild: true, wantPush: true, wantInspect: true, wantTags: []string{"v0.1.10"}, annotationVersion: "v0.1.10", annotationHeading: "**Published oci-accelerator v0.1.10**"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			git := func(dir string, args ...string) string { return imagePipelineGit(t, dir, args...) }
			root := t.TempDir()
			checkout := tc.checkout
			if checkout == "" {
				checkout = "main"
			}
			git(root, "clone", "--bare", origin, "origin")
			git(root, "clone", "--depth", "1", "--branch", checkout, "file://"+filepath.Join(root, "origin"), "repo")
			repo := filepath.Join(root, "repo")
			if tc.version != "" {
				writeFile(t, filepath.Join(repo, "VERSION"), tc.version, 0o600)
				git(repo, "add", "VERSION")
				git(repo, "commit", "-am", "change version")
			}
			if tc.failure == "main-tip" || tc.failure == "published-tip" {
				writeFile(t, filepath.Join(repo, "VERSION"), "v0.2.0\n", 0o600)
				git(repo, "commit", "-am", "release at tip")
				git(repo, "push", "origin", "main")
			}
			if tc.failure == "extra-commit" {
				git(repo, "commit", "--allow-empty", "-m", "later PR change")
			}
			if tc.failure == "missing-version" {
				git(repo, "rm", "VERSION")
				git(repo, "commit", "-m", "remove version")
			}
			git(repo, "push", "origin", "HEAD:refs/heads/fixture")
			if checkout == "unprefixed" {
				git(filepath.Join(root, "origin"), "update-ref", "refs/heads/main", git(repo, "rev-parse", "HEAD"))
			}

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
			bin := installImagePipelineMocks(t, root)
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
				"BUILDKITE_BUILD_NUMBER="+number, "BUILDKITE_BUILD_ID=fixture-build-id", "IMAGE_TEST_ROOT="+root, "IMAGE_TEST_FAILURE="+tc.failure)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v: %s", err, tc.wantError, out)
			}
			if strings.Contains(string(out), "fixture-token") {
				t.Fatal("credential appeared in output")
			}
			if tc.failure == "annotation" && !strings.Contains(string(out), "Warning:") {
				t.Fatalf("annotation failure output lacks warning: %s", out)
			}
			_, err = os.Stat(filepath.Join(root, "secret-calls"))
			if (err == nil) != tc.wantSecret {
				t.Fatalf("secret retrieved = %v, want %v", err == nil, tc.wantSecret)
			}
			if !tc.wantInspect {
				secrets := readImageFixtureLog(t, root, "secret-calls")
				operations := readImageFixtureLog(t, root, "operations")
				if strings.Contains(secrets, "OCI_ACCELERATOR_GITHUB_RELEASE_TOKEN") || strings.Contains(operations, "artifact ") || readImageFixtureLog(t, root, "gh-calls") != "" {
					t.Fatal("non-release build accessed GitHub release credentials, artifacts, or API")
				}
			}
			calls, _ := os.ReadFile(filepath.Join(root, "docker-calls"))
			if strings.Contains(string(calls), "buildx build ") != tc.wantBuild || strings.Contains(string(calls), "--push") != tc.wantPush {
				t.Fatalf("unexpected Docker operations: %s", calls)
			}
			if tc.wantBuild && !strings.Contains(string(calls), "--platform linux/amd64,linux/arm64") {
				t.Fatalf("missing platform pair: %s", calls)
			}
			wantInspections := 0
			if tc.wantInspect {
				wantInspections = 1
			}
			if got := strings.Count(string(calls), "buildx imagetools inspect "); got != wantInspections {
				t.Fatalf("registry inspections = %d, want %d: %s", got, wantInspections, calls)
			}
			if strings.Count(string(calls), "--tag ") != len(tc.wantTags) {
				t.Fatalf("expected only tags %v: %s", tc.wantTags, calls)
			}
			for _, tag := range tc.wantTags {
				if !strings.Contains(string(calls), "--tag docker.io/buildkite/oci-accelerator:"+tag+" ") {
					t.Fatalf("missing tag %q: %s", tag, calls)
				}
			}
			if tc.wantBuild {
				version, err := os.ReadFile(filepath.Join(repo, "VERSION"))
				if err != nil {
					t.Fatal(err)
				}
				for _, flag := range []string{"--label ", "--annotation index:"} {
					for _, metadata := range []string{"version=" + strings.TrimSpace(string(version)), "revision=" + commit} {
						if !strings.Contains(string(calls), flag+"org.opencontainers.image."+metadata+" ") {
							t.Fatalf("missing image metadata %q: %s", metadata, calls)
						}
					}
				}
			}
			annotationCalls, _ := os.ReadFile(filepath.Join(root, "annotate-calls"))
			wantAnnotations := 0
			if tc.annotationVersion != "" {
				wantAnnotations = 1
			}
			if got := strings.Count(string(annotationCalls), "annotate "); got != wantAnnotations {
				t.Fatalf("annotation calls = %d, want %d: %s", got, wantAnnotations, annotationCalls)
			}
			if tc.annotationVersion != "" {
				if got := strings.TrimSpace(string(annotationCalls)); got != "annotate --style success --context oci-accelerator-release" {
					t.Fatalf("annotation args = %q", got)
				}
				body, err := os.ReadFile(filepath.Join(root, "annotation-body"))
				if err != nil {
					t.Fatal(err)
				}
				for _, expected := range []string{
					tc.annotationHeading,
					"docker.io/buildkite/oci-accelerator:" + tc.annotationVersion,
					"linux/amd64",
					"linux/arm64",
					"https://hub.docker.com/r/buildkite/oci-accelerator/tags?name=" + tc.annotationVersion,
					"https://github.com/buildkite/oci-accelerator/commit/" + commit,
					"```sh\ndocker pull docker.io/buildkite/oci-accelerator:" + tc.annotationVersion + "\n```",
				} {
					if !strings.Contains(string(body), expected) {
						t.Fatalf("annotation body missing %q:\n%s", expected, body)
					}
				}
				operations, err := os.ReadFile(filepath.Join(root, "operations"))
				if err != nil {
					t.Fatal(err)
				}
				before := "docker buildx build "
				if strings.HasPrefix(tc.annotationHeading, "**Version ") && !tc.wantBuild {
					before = "docker buildx imagetools inspect "
				}
				beforeIndex := strings.LastIndex(string(operations), before)
				annotationIndex := strings.Index(string(operations), "annotate annotate ")
				if beforeIndex < 0 || annotationIndex < 0 || beforeIndex > annotationIndex {
					t.Fatalf("annotation occurred before successful build or inspection:\n%s", operations)
				}
			}
			if config, err := os.ReadFile(filepath.Join(root, "docker-config")); err == nil {
				if _, err := os.Stat(strings.TrimSpace(string(config))); !os.IsNotExist(err) {
					t.Fatalf("temporary Docker credentials not cleaned: %v", err)
				}
			}
		})
	}
}
