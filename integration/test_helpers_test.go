package integration

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type githubReleaseFixture struct {
	Release *fixtureRelease `json:"release"`
	Assets  []fixtureAsset  `json:"assets"`
}

type fixtureRelease struct {
	ID              int    `json:"id"`
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	Name            string `json:"name"`
	Draft           bool   `json:"draft"`
	Prerelease      bool   `json:"prerelease"`
}

type fixtureAsset struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Bytes []byte `json:"fixture_bytes"`
}

// Run the test executable as a network-free gh and artifact-download subprocess.
// State is persisted outside the checkout so retries observe actual prior writes.
func TestGitHubReleaseCLIHelper(t *testing.T) {
	mode := os.Getenv("IMAGE_TEST_CLI")
	if mode == "" {
		return
	}

	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "missing helper arguments")
		os.Exit(99)
	}

	if err := runGitHubReleaseCLI(mode, args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runGitHubReleaseCLI(mode string, args []string) error {
	root := os.Getenv("IMAGE_TEST_ROOT")
	failure := os.Getenv("IMAGE_TEST_FAILURE")
	fail := func(message string) error {
		if err := os.WriteFile(filepath.Join(root, "failure-observed"), []byte(failure), 0o600); err != nil {
			return err
		}

		return fmt.Errorf("%s", message)
	}

	versionBytes, err := os.ReadFile("VERSION")
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(versionBytes))
	if mode == "artifacts" {
		if len(args) != 8 || !reflect.DeepEqual(args[:7], []string{"artifact", "download", "--build", "fixture-build-id", "--step", "binaries", "*"}) || filepath.Base(args[7]) != "releases" {
			return fmt.Errorf("unexpected artifact download: %q", args)
		}
		if failure == "artifacts" {
			return fail("artifact download interrupted")
		}
		if err := os.MkdirAll(args[7], 0o700); err != nil {
			return err
		}

		files := githubReleaseArtifacts(version)
		entries, err := os.ReadDir(filepath.Join(root, "artifacts"))
		if err == nil {
			files = map[string][]byte{}
			for _, entry := range entries {
				data, err := os.ReadFile(filepath.Join(root, "artifacts", entry.Name()))
				if err != nil {
					return err
				}
				files[entry.Name()] = data
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if failure == "extra-artifact" {
			files["unexpected.tar.gz"] = []byte("unexpected")
		}
		if failure == "missing-artifact" {
			delete(files, "oci-accelerator-linux-amd64-"+version+".tar.gz")
		}
		if failure == "bad-checksum" {
			files["oci-accelerator-linux-amd64-"+version+".tar.gz"] = []byte("corrupt download")
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(args[7], name), data, 0o600); err != nil {
				return err
			}
		}
		return nil
	}

	if os.Getenv("GH_TOKEN") != "github-fixture-token" {
		return fmt.Errorf("gh did not receive the release credential")
	}
	for _, name := range []string{"gh-calls", "operations"} {
		f, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		fmt.Fprintln(f, "gh "+strings.Join(args, " "))
		f.Close()
	}

	if len(args) < 4 || !reflect.DeepEqual(args[:3], []string{"api", "--hostname", "github.com"}) {
		return fmt.Errorf("unexpected gh invocation: %q", args)
	}
	method, endpoint, input := "GET", "", ""
	fields := map[string]string{}
	var headers []string
	paginate, slurp := false, false
	for i := 3; i < len(args); i++ {
		switch args[i] {
		case "--paginate":
			paginate = true
		case "--slurp":
			slurp = true
		case "--method", "-X", "--input", "--header", "-H", "-f", "-F":
			flag := args[i]
			i++
			if i == len(args) {
				return fmt.Errorf("missing value for %s", flag)
			}
			switch flag {
			case "--method", "-X":
				method = args[i]
			case "--input":
				input = args[i]
			case "--header", "-H":
				headers = append(headers, args[i])
			default:
				key, value, ok := strings.Cut(args[i], "=")
				if !ok {
					return fmt.Errorf("invalid field: %s", args[i])
				}
				fields[key] = value
			}
		default:
			if endpoint != "" {
				return fmt.Errorf("unexpected gh argument: %s", args[i])
			}
			endpoint = args[i]
		}
	}

	var state githubReleaseFixture
	data, err := os.ReadFile(filepath.Join(root, "github-state.json"))
	if err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	emit := func(value any) error { return json.NewEncoder(os.Stdout).Encode(value) }
	save := func() error {
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}

		return os.WriteFile(filepath.Join(root, "github-state.json"), data, 0o600)
	}

	base := "repos/buildkite/oci-accelerator"
	switch {
	case method == "GET" && endpoint == base+"/releases?per_page=100":
		if !paginate || !slurp {
			return fmt.Errorf("release listing must paginate and slurp")
		}
		if failure == "gh-list" {
			return fail("GitHub connection timed out")
		}

		releases := []*fixtureRelease{}
		if state.Release != nil {
			releases = append(releases, state.Release)
		}
		if failure == "pagination" {
			return emit([]any{[]fixtureRelease{{ID: 1, TagName: "v0.0.1"}}, releases})
		}
		return emit([]any{releases})
	case method == "GET" && endpoint == base+"/releases/42/assets?per_page=100":
		if !paginate || !slurp {
			return fmt.Errorf("asset listing must paginate and slurp")
		}
		if failure == "gh-assets" {
			return fail("HTTP 401: Bad credentials")
		}

		assets := make([]map[string]any, 0, len(state.Assets))
		for _, asset := range state.Assets {
			assets = append(assets, githubAssetResponse(asset))
		}
		if failure == "pagination" && len(assets) > 1 {
			return emit([]any{assets[:1], assets[1:]})
		}
		return emit([]any{assets})
	case method == "POST" && endpoint == base+"/releases":
		want := map[string]string{"tag_name": version, "target_commitish": os.Getenv("BUILDKITE_COMMIT"), "name": version, "draft": "true", "generate_release_notes": "true"}
		if state.Release != nil || !reflect.DeepEqual(fields, want) {
			return fmt.Errorf("invalid release creation: %v", fields)
		}
		if failure == "gh-create" {
			return fail("HTTP 403: release creation forbidden")
		}

		state.Release = &fixtureRelease{ID: 42, TagName: version, TargetCommitish: fields["target_commitish"], Name: version, Draft: true}
		if err := save(); err != nil {
			return err
		}
		return emit(state.Release)
	case method == "POST" && strings.HasPrefix(endpoint, "https://uploads.github.com/"+base+"/releases/42/assets?name="):
		if state.Release == nil || !state.Release.Draft || !reflect.DeepEqual(headers, []string{"Content-Type: application/octet-stream"}) {
			return fmt.Errorf("invalid asset upload")
		}

		u, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		name := u.Query().Get("name")
		for _, asset := range state.Assets {
			if asset.Name == name {
				return fmt.Errorf("refusing duplicate asset upload: %s", name)
			}
		}
		if failure == "gh-upload" && len(state.Assets) == 1 {
			return fail("upload connection interrupted")
		}

		data, err := os.ReadFile(input)
		if err != nil {
			return err
		}
		asset := fixtureAsset{ID: 200 + len(state.Assets), Name: name, State: "uploaded", Bytes: data}
		state.Assets = append(state.Assets, asset)
		if err := save(); err != nil {
			return err
		}
		return emit(githubAssetResponse(asset))
	case method == "GET" && strings.HasPrefix(endpoint, base+"/releases/assets/"):
		if !reflect.DeepEqual(headers, []string{"Accept: application/octet-stream"}) {
			return fmt.Errorf("asset download must request exact bytes")
		}
		if failure == "gh-download" {
			return fail("asset download connection reset")
		}

		id, _ := strconv.Atoi(strings.TrimPrefix(endpoint, base+"/releases/assets/"))
		for _, asset := range state.Assets {
			if asset.ID == id {
				_, err := os.Stdout.Write(asset.Bytes)
				return err
			}
		}
		return fmt.Errorf("unknown asset: %d", id)
	case method == "POST" && endpoint == base+"/git/refs":
		if !reflect.DeepEqual(fields, map[string]string{"ref": "refs/tags/" + version, "sha": os.Getenv("BUILDKITE_COMMIT")}) {
			return fmt.Errorf("incorrect tag creation: %v", fields)
		}
		if failure == "gh-tag" {
			return fail("tag creation unavailable")
		}

		cmd := exec.Command("git", "--git-dir="+filepath.Join(root, "origin"), "update-ref", fields["ref"], fields["sha"], strings.Repeat("0", 40))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("create real origin tag: %w: %s", err, out)
		}
		return emit(map[string]any{"ref": fields["ref"], "object": map[string]string{"sha": fields["sha"]}})
	case method == "PATCH" && endpoint == base+"/releases/42":
		if state.Release == nil || !state.Release.Draft || !reflect.DeepEqual(fields, map[string]string{"draft": "false", "make_latest": "legacy"}) {
			return fmt.Errorf("invalid release publication: %v", fields)
		}
		if failure == "gh-publish" {
			return fail("publication unavailable")
		}

		state.Release.Draft = false
		if err := save(); err != nil {
			return err
		}
		if failure == "lost-publish" {
			return fail("publication response lost")
		}
		return emit(state.Release)
	default:
		return fmt.Errorf("unexpected GitHub API call: %s %s", method, endpoint)
	}
}

func githubReleaseArtifacts(version string) map[string][]byte {
	files := map[string][]byte{}
	for _, platform := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"} {
		files["oci-accelerator-"+platform+"-"+version+".tar.gz"] = []byte("archive\x00" + platform + "\n" + version + "\n")
	}

	var manifest strings.Builder
	for _, name := range sortedArtifactNames(files) {
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(files[name]), name)
	}
	files["oci-accelerator-"+version+".SHA256SUMS"] = []byte(manifest.String())
	return files
}

func githubAssetResponse(asset fixtureAsset) map[string]any {
	return map[string]any{"id": asset.ID, "name": asset.Name, "state": asset.State, "size": len(asset.Bytes)}
}

func runImagePipeline(t *testing.T, script, repo, root, bin, failure string, imagePublished bool) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", script, "publish")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "IMAGE_TEST_ROOT="+root,
		"IMAGE_TEST_FAILURE="+failure, "IMAGE_TEST_PUBLISHED="+strconv.FormatBool(imagePublished),
		"BUILDKITE_BRANCH=main", "BUILDKITE_TAG=", "BUILDKITE_PULL_REQUEST=false",
		"BUILDKITE_COMMIT="+imagePipelineGit(t, repo, "rev-parse", "HEAD"),
		"BUILDKITE_BUILD_NUMBER=108", "BUILDKITE_BUILD_ID=fixture-build-id")
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "fixture-token") {
		t.Fatal("credential appeared in output")
	}

	return string(out), err
}

func readGitHubReleaseFixture(t *testing.T, root string) githubReleaseFixture {
	t.Helper()
	var state githubReleaseFixture
	data, err := os.ReadFile(filepath.Join(root, "github-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}

	return state
}

func sortedArtifactNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func readImageFixtureLog(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return string(data)
}

func imagePipelineGit(t *testing.T, dir string, args ...string) string {
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

func imagePipelineHistory(t *testing.T) string {
	t.Helper()
	git := func(dir string, args ...string) string { return imagePipelineGit(t, dir, args...) }
	origin := t.TempDir()
	git(origin, "init", "-b", "main")
	git(origin, "commit", "--allow-empty", "-m", "before VERSION")
	git(origin, "branch", "before-version")

	git(origin, "checkout", "-b", "unprefixed")
	writeFile(t, filepath.Join(origin, "VERSION"), "0.1.0\n", 0o600)
	git(origin, "add", "VERSION")
	git(origin, "commit", "-m", "unsupported version format")

	git(origin, "checkout", "main")
	writeFile(t, filepath.Join(origin, "VERSION"), "v0.1.0\n", 0o600)
	git(origin, "add", "VERSION")
	git(origin, "commit", "-m", "first release")
	git(origin, "branch", "first-release")
	writeFile(t, filepath.Join(origin, "VERSION"), "v0.1.9\n", 0o600)
	git(origin, "commit", "-am", "previous version")
	git(origin, "commit", "--allow-empty", "-m", "ordinary main")
	git(origin, "branch", "older")

	git(origin, "checkout", "-b", "release-pr")
	writeFile(t, filepath.Join(origin, "VERSION"), "v0.1.10\n", 0o600)
	git(origin, "commit", "-am", "bump version")
	git(origin, "checkout", "main")
	git(origin, "merge", "--no-ff", "release-pr", "-m", "merge release")
	git(origin, "branch", "release")
	git(origin, "commit", "--allow-empty", "-m", "later main")
	git(origin, "checkout", "-b", "unmerged")
	git(origin, "commit", "--allow-empty", "-m", "unmerged")
	git(origin, "checkout", "main")

	return origin
}

func installImagePipelineMocks(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Race-instrumented helpers are synchronous, short-lived CLI processes.
	// Avoid the race runtime's one-second exit delay on every API request.
	helper := "export GORACE=atexit_sleep_ms=0\nexec " + strconv.Quote(executable) + " -test.run=^TestGitHubReleaseCLIHelper$ -- \"$@\"\n"
	writeFile(t, filepath.Join(bin, "gh"), "#!/usr/bin/env bash\nexport IMAGE_TEST_CLI=gh\n"+helper, 0o700)
	writeFile(t, filepath.Join(bin, "artifact-helper"), "#!/usr/bin/env bash\nexport IMAGE_TEST_CLI=artifacts\n"+helper, 0o700)

	writeFile(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$IMAGE_TEST_ROOT/docker-calls"
printf 'docker %s\n' "$*" >> "$IMAGE_TEST_ROOT/operations"
printf '%s\n' "$DOCKER_CONFIG" > "$IMAGE_TEST_ROOT/docker-config"
case "$1 $2" in
  'login --username')
    [[ "$*" == 'login --username buildkite --password-stdin docker.io' ]]
    [[ $(cat) == fixture-token ]]
    [[ "$IMAGE_TEST_FAILURE" != login ]]
    ;;
  'buildx imagetools')
    [[ "$3" == inspect ]]
    if [[ ${IMAGE_TEST_PUBLISHED:-false} == true || -f "$IMAGE_TEST_ROOT/image-published" ]]; then
      printf '%s %s' "$BUILDKITE_COMMIT" "$(cat VERSION)"
      exit 0
    fi
    case "$IMAGE_TEST_FAILURE" in
      published|published-tip) printf '%s %s' "$BUILDKITE_COMMIT" "$(cat VERSION)" ;;
      conflict) printf 'different-commit %s' "$(cat VERSION)" ;;
      no-metadata) printf ' ' ;;
      unauthorized) echo 'ERROR: authorization failed' >&2; exit 1 ;;
      timeout) echo 'ERROR: connection timed out' >&2; exit 1 ;;
      unknown) echo 'ERROR: unexpected not found response' >&2; exit 1 ;;
      *) echo "ERROR: $4: not found" >&2; exit 1 ;;
    esac
    ;;
  'buildx build')
    [[ "$IMAGE_TEST_FAILURE" != build ]]
    if [[ "$*" == *'--push'* ]]; then touch "$IMAGE_TEST_ROOT/image-published"; fi
    ;;
  'buildx create'|'buildx rm') ;;
  *) exit 99 ;;
esac
`, 0o700)

	writeFile(t, filepath.Join(bin, "buildkite-agent"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  secret)
    printf '%s\n' "$*" >> "$IMAGE_TEST_ROOT/secret-calls"
    printf 'secret %s\n' "$*" >> "$IMAGE_TEST_ROOT/operations"
    case "$*" in
      'secret get --format default OCI_ACCELERATOR_DOCKER_HUB_TOKEN')
        [[ "$IMAGE_TEST_FAILURE" != secret ]]
        printf fixture-token
        ;;
      'secret get --format default OCI_ACCELERATOR_GITHUB_RELEASE_TOKEN')
        if [[ "$IMAGE_TEST_FAILURE" == gh-secret ]]; then
          printf gh-secret > "$IMAGE_TEST_ROOT/failure-observed"
          exit 1
        fi
        printf github-fixture-token
        ;;
      *) exit 99 ;;
    esac
    ;;
  artifact)
    printf 'artifact %s\n' "$*" >> "$IMAGE_TEST_ROOT/operations"
    exec artifact-helper "$@"
    ;;
  annotate)
    printf '%s\n' "$*" >> "$IMAGE_TEST_ROOT/annotate-calls"
    printf 'annotate %s\n' "$*" >> "$IMAGE_TEST_ROOT/operations"
    cat > "$IMAGE_TEST_ROOT/annotation-body"
    [[ "$IMAGE_TEST_FAILURE" != annotation ]]
    ;;
  *) exit 99 ;;
esac
`, 0o700)

	return bin
}
