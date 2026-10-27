package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGitHubReleasePipeline(t *testing.T) {
	requireCommands(t, "bash", "git", "jq", "sha256sum")

	script, err := filepath.Abs("../.buildkite/image.sh")
	if err != nil {
		t.Fatal(err)
	}

	origin := imagePipelineHistory(t)
	for _, tc := range []struct {
		name, existing, assets, tag, failure string
		wantError, wantBuild, retry, tip     bool
	}{
		{name: "fresh release after main advances", wantBuild: true},
		{name: "fresh release at main tip", wantBuild: true, tip: true},
		{name: "published retry", existing: "published", tag: "matching", assets: "complete"},
		{name: "published retry with annotated tag", existing: "published", tag: "annotated", assets: "complete"},
		{name: "partial draft retry", existing: "draft", assets: "partial", wantBuild: true},
		{name: "paginated draft and assets", existing: "draft", assets: "partial", failure: "pagination", wantBuild: true},
		{name: "draft target mismatch", existing: "wrong-target", wantError: true},
		{name: "conflicting lightweight tag", tag: "conflict", wantError: true},
		{name: "conflicting annotated tag", tag: "annotated-conflict", wantError: true},
		{name: "published missing tag", existing: "published", assets: "complete", wantError: true},
		{name: "published missing asset", existing: "published", assets: "partial", tag: "matching", wantError: true},
		{name: "published mismatched bytes", existing: "published", assets: "corrupt", tag: "matching", wantError: true},
		{name: "draft mismatched bytes", existing: "draft", assets: "corrupt", wantError: true},
		{name: "draft interrupted asset", existing: "draft", assets: "starter", wantError: true},
		{name: "release list network failure", failure: "gh-list", wantError: true},
		{name: "asset list authorization failure", existing: "draft", failure: "gh-assets", wantError: true},
		{name: "asset download failure", existing: "draft", assets: "complete", failure: "gh-download", wantError: true},
		{name: "release creation failure", failure: "gh-create", wantError: true},
		{name: "release secret failure", failure: "gh-secret", wantError: true},
		{name: "artifact download failure", failure: "artifacts", wantError: true},
		{name: "extra artifact rejected", failure: "extra-artifact", wantError: true},
		{name: "missing archive rejected", failure: "missing-artifact", wantError: true},
		{name: "bad checksum rejected", failure: "bad-checksum", wantError: true},
		{name: "upload interrupted then resumed", failure: "gh-upload", wantError: true, retry: true},
		{name: "image failure leaves draft then resumes", failure: "build", wantError: true, wantBuild: true, retry: true},
		{name: "tag API failure then resumes", failure: "gh-tag", wantError: true, wantBuild: true, retry: true},
		{name: "publish API failure then resumes", failure: "gh-publish", wantError: true, wantBuild: true, retry: true},
		{name: "lost publish response then read only retry", failure: "lost-publish", wantError: true, wantBuild: true, retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			imagePipelineGit(t, root, "clone", "--bare", origin, "origin")
			branch := "release"
			if tc.tip {
				branch = "main"
			}
			imagePipelineGit(t, root, "clone", "--depth", "1", "--branch", branch, "file://"+filepath.Join(root, "origin"), "repo")
			repo := filepath.Join(root, "repo")
			if tc.tip {
				writeFile(t, filepath.Join(repo, "VERSION"), "v0.2.0\n", 0o600)
				imagePipelineGit(t, repo, "commit", "-am", "release at main tip")
				imagePipelineGit(t, repo, "push", "origin", "main")
			}
			head := imagePipelineGit(t, repo, "rev-parse", "HEAD")
			versionBytes, err := os.ReadFile(filepath.Join(repo, "VERSION"))
			if err != nil {
				t.Fatal(err)
			}

			version := strings.TrimSpace(string(versionBytes))
			files := githubReleaseArtifacts(version)
			state := githubReleaseFixture{}
			if tc.existing != "" {
				state.Release = &fixtureRelease{ID: 42, TagName: version, TargetCommitish: head, Name: version, Draft: tc.existing != "published"}
				if tc.existing == "wrong-target" {
					state.Release.TargetCommitish = "main"
				}
			}
			for i, name := range sortedArtifactNames(files) {
				if tc.assets == "" || (tc.assets == "partial" && i > 1) {
					continue
				}
				asset := fixtureAsset{ID: 100 + i, Name: name, State: "uploaded", Bytes: files[name]}
				if tc.assets == "corrupt" && i == 0 {
					asset.Bytes = []byte("different bytes, same asset name")
				}
				if tc.assets == "starter" && i == 0 {
					asset.State = "starter"
				}
				state.Assets = append(state.Assets, asset)
			}

			if tc.tag != "" {
				target := head
				if strings.Contains(tc.tag, "conflict") {
					target = imagePipelineGit(t, filepath.Join(root, "origin"), "rev-parse", "refs/heads/older")
				}
				if strings.HasPrefix(tc.tag, "annotated") {
					imagePipelineGit(t, filepath.Join(root, "origin"), "tag", "-a", version, target, "-m", "release")
				} else {
					imagePipelineGit(t, filepath.Join(root, "origin"), "update-ref", "refs/tags/"+version, target)
				}
			}
			tagsBefore := imagePipelineGit(t, filepath.Join(root, "origin"), "for-each-ref", "--format=%(objectname) %(refname)", "refs/tags/")
			writeGitHubReleaseFixture(t, root, state)
			bin := installImagePipelineMocks(t, root)

			out, err := runImagePipeline(t, script, repo, root, bin, tc.failure, tc.existing == "published")
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v:\n%s\n%s", err, tc.wantError, out, readImageFixtureLog(t, root, "operations"))
			}
			if strings.HasPrefix(tc.failure, "gh-") || tc.failure == "lost-publish" || tc.failure == "artifacts" {
				if got := readImageFixtureLog(t, root, "failure-observed"); got != tc.failure {
					t.Fatalf("injected failure %q was not reached: %q\n%s", tc.failure, got, out)
				}
			}
			calls := readImageFixtureLog(t, root, "docker-calls")
			if strings.Contains(calls, "buildx build ") != tc.wantBuild {
				t.Fatalf("image build = %v, want %v:\n%s", strings.Contains(calls, "buildx build "), tc.wantBuild, calls)
			}
			if !tc.tip && strings.Contains(calls, ":latest") {
				t.Fatalf("advanced main release moved latest:\n%s", calls)
			}

			if tc.retry {
				afterFailure := readGitHubReleaseFixture(t, root)
				if afterFailure.Release == nil || afterFailure.Release.Draft != (tc.failure != "lost-publish") {
					t.Fatalf("unexpected release after interruption: %+v", afterFailure.Release)
				}
				before := readImageFixtureLog(t, root, "gh-calls")
				imagePublished := tc.failure == "gh-tag" || tc.failure == "gh-publish" || tc.failure == "lost-publish"
				if out, err := runImagePipeline(t, script, repo, root, bin, "", imagePublished); err != nil {
					t.Fatalf("retry failed: %v:\n%s", err, out)
				}
				if tc.failure == "lost-publish" {
					retryCalls := strings.TrimPrefix(readImageFixtureLog(t, root, "gh-calls"), before)
					if strings.Contains(retryCalls, "--method") {
						t.Fatalf("published retry mutated GitHub:\n%s", retryCalls)
					}
				}
			}

			final := readGitHubReleaseFixture(t, root)
			if tc.wantError && !tc.retry {
				if !reflect.DeepEqual(final, state) {
					t.Fatalf("failed preflight mutated release: before %+v, after %+v", state, final)
				}
				if tagsAfter := imagePipelineGit(t, filepath.Join(root, "origin"), "for-each-ref", "--format=%(objectname) %(refname)", "refs/tags/"); tagsAfter != tagsBefore {
					t.Fatalf("failed preflight mutated tags: before %q, after %q", tagsBefore, tagsAfter)
				}
			}
			if !tc.wantError || tc.retry {
				if final.Release == nil || final.Release.Draft || final.Release.TagName != version || final.Release.TargetCommitish != head {
					t.Fatalf("release not published correctly: %+v", final.Release)
				}
				if len(final.Assets) != len(files) {
					t.Fatalf("assets = %d, want %d", len(final.Assets), len(files))
				}
				for _, asset := range final.Assets {
					if !bytes.Equal(asset.Bytes, files[asset.Name]) || asset.State != "uploaded" {
						t.Fatalf("incorrect asset %q", asset.Name)
					}
				}
				if got := imagePipelineGit(t, filepath.Join(root, "origin"), "rev-parse", "refs/tags/"+version+"^{}"); got != head {
					t.Fatalf("tag points to %s, want %s", got, head)
				}
				operations := readImageFixtureLog(t, root, "operations")
				if tc.wantBuild && !tc.retry {
					build := strings.Index(operations, "docker buildx build ")
					publish := strings.Index(operations, "gh api --hostname github.com --method PATCH ")
					if build < 0 || publish < build {
						t.Fatalf("release published before images:\n%s", operations)
					}
				}
			}
			if tc.existing == "published" && strings.Contains(readImageFixtureLog(t, root, "gh-calls"), "--method") {
				t.Fatal("published release retry attempted a GitHub mutation")
			}
			for _, old := range state.Assets {
				for _, asset := range final.Assets {
					if asset.Name == old.Name && !reflect.DeepEqual(asset, old) {
						t.Fatalf("existing asset was overwritten: %q", old.Name)
					}
				}
			}
		})
	}
}

func writeGitHubReleaseFixture(t *testing.T, root string, state githubReleaseFixture) {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, "github-state.json"), string(data), 0o600)
}
