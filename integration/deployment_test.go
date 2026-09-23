package integration

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/oci-accelerator/internal/config"
	"go.yaml.in/yaml/v3"
)

func TestPlainDeploymentSafety(t *testing.T) {
	checkDeployment(t, plainDeployment(t), "oci-accelerator-bootstrap", false)
}

func TestHelmDeploymentParity(t *testing.T) {
	requireHelm(t)
	plain := plainDeployment(t)
	helm := renderChart(t)
	checkDeployment(t, helm, "oci-accelerator-bootstrap-1", false)
	if !reflect.DeepEqual(deploymentConfig(t, plain), deploymentConfig(t, helm)) {
		t.Fatal("Helm and plain manifests resolve to different default application configurations")
	}
	for _, kind := range []string{"Deployment", "Service", "NetworkPolicy", "Role", "PersistentVolumeClaim"} {
		name := "oci-accelerator"
		if kind == "Role" {
			name += "-bootstrap"
		} else if kind == "PersistentVolumeClaim" {
			name += "-cache"
		}
		if kind == "Deployment" {
			// Compare operational contracts rather than incidental YAML ordering.
			for _, field := range []string{"startupProbe", "readinessProbe", "livenessProbe", "resources", "securityContext"} {
				a := proxyContainer(t, plain)
				b := proxyContainer(t, helm)
				if !reflect.DeepEqual(a[field], b[field]) {
					t.Errorf("proxy %s differs between packages", field)
				}
			}
		} else if kind == "Role" {
			if !reflect.DeepEqual(plain[kind+"/"+name]["rules"], helm[kind+"/"+name]["rules"]) {
				t.Error("bootstrap permissions differ between packages")
			}
		} else {
			if !reflect.DeepEqual(plain[kind+"/"+name]["spec"], helm[kind+"/"+name]["spec"]) {
				t.Errorf("default %s spec differs between packages", kind)
			}
		}
	}
}

func TestHelmDeploymentOptions(t *testing.T) {
	requireHelm(t)
	remote := renderChart(t, "--set", "metrics.enabled=true,metrics.serviceMonitor.enabled=true,metrics.namespace=observability,buildNamespace=builders")
	checkDeployment(t, remote, "oci-accelerator-bootstrap-1", true)
	policy := nested(remote["NetworkPolicy/oci-accelerator"], "spec")["ingress"].([]any)
	for i, namespace := range []string{"builders", "observability"} {
		rule := policy[i].(map[string]any)
		from := rule["from"].([]any)[0].(map[string]any)
		if nested(from, "namespaceSelector", "matchLabels")["kubernetes.io/metadata.name"] != namespace {
			t.Fatalf("rule %d permits the wrong namespace", i)
		}
	}
	if remote["ServiceMonitor/oci-accelerator"] == nil {
		t.Fatal("enabled ServiceMonitor missing")
	}
	existing := renderChart(t, "--set", "cache.existingClaim=retained-cache")
	if existing["PersistentVolumeClaim/oci-accelerator-cache"] != nil {
		t.Fatal("existing claim option created another PVC")
	}
	volumes := nested(existing["Deployment/oci-accelerator"], "spec", "template", "spec")["volumes"].([]any)
	if nested(named(t, volumes, "cache"), "persistentVolumeClaim")["claimName"] != "retained-cache" {
		t.Fatal("existing PVC not mounted")
	}
	for _, args := range [][]string{
		{}, {"--set", "image.digest=sha256:invalid"},
		{"--set", "image.tag=dev,metrics.serviceMonitor.enabled=true"},
	} {
		cmd := exec.Command("helm", append([]string{"template", "oci-accelerator", "../deploy/helm/oci-accelerator"}, args...)...)
		if output, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("invalid values accepted: %v (%s)", args, output)
		}
	}
}

func TestHelmBootstrapNamesStayBoundedAcrossRevisions(t *testing.T) {
	requireHelm(t)
	// Render the chart's actual naming helper with a future revision, rather
	// than reproducing its truncation/hash algorithm in the assertion.
	chart := t.TempDir()
	if err := os.Mkdir(filepath.Join(chart, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile("../deploy/helm/oci-accelerator/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"Chart.yaml":             []byte("apiVersion: v2\nname: name-test\nversion: 0.1.0\n"),
		"templates/_helpers.tpl": helper,
		"templates/job.yaml": []byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: {{ include "oci-accelerator.bootstrapJobName" (dict "Release" (dict "Name" .Release.Name "Revision" 2147483647)) }}
`),
	} {
		if err := os.WriteFile(filepath.Join(chart, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	for _, release := range []string{strings.Repeat("a", 53), strings.Repeat("a", 52) + "b"} {
		output, err := exec.Command("helm", "template", release, chart).CombinedOutput()
		if err != nil {
			t.Fatalf("long release name: %v: %s", err, output)
		}
		for _, object := range decodeDeployment(t, output) {
			name := nested(object, "metadata")["name"].(string)
			if len(name) > 63 || !strings.HasSuffix(name, "-bootstrap-2147483647") || seen[name] {
				t.Fatalf("invalid or colliding Job name %q", name)
			}
			seen[name] = true
		}
	}
}

func checkDeployment(t *testing.T, objects map[string]map[string]any, jobName string, remote bool) {
	t.Helper()
	deployment := objects["Deployment/oci-accelerator"]
	spec := nested(deployment, "spec")
	if spec["replicas"] != 1 || nested(spec, "strategy")["type"] != "Recreate" {
		t.Fatal("single-owner cache requires one replica and Recreate")
	}
	pod := nested(spec, "template", "spec")
	if pod["automountServiceAccountToken"] != false || nested(pod, "securityContext")["fsGroup"] != nil {
		t.Fatal("serving pod must have no API token or fsGroup permission mutation")
	}
	if objects["Secret/oci-accelerator-ca"] != nil || objects["ConfigMap/oci-accelerator-trust"] != nil {
		t.Fatal("CA identity must be owned by bootstrap, not deployment templates")
	}
	proxy := proxyContainer(t, objects)
	security := nested(proxy, "securityContext")
	if security["runAsUser"] != 10001 || security["runAsNonRoot"] != true || security["readOnlyRootFilesystem"] != true || security["allowPrivilegeEscalation"] != false {
		t.Fatal("unsafe serving container security context")
	}
	for _, mount := range proxy["volumeMounts"].([]any) {
		if mount.(map[string]any)["name"] == "ca-secret" {
			t.Fatal("serving container directly mounts the projected private Secret")
		}
	}
	bootstrap := nested(objects["Job/"+jobName], "spec", "template", "spec")
	if bootstrap["serviceAccountName"] != "oci-accelerator-bootstrap" || bootstrap["automountServiceAccountToken"] != true {
		t.Fatal("bootstrap must use its separate API identity")
	}
	ports := nested(objects["Service/oci-accelerator"], "spec")["ports"].([]any)
	wantPorts := 1
	if remote {
		wantPorts = 2
	}
	if len(ports) != wantPorts || ports[0].(map[string]any)["port"] != 3128 {
		t.Fatal("unexpected service exposure")
	}
	cfg := deploymentConfig(t, objects)
	wantMetrics := "127.0.0.1:9090"
	if remote {
		wantMetrics = "0.0.0.0:9090"
	}
	if cfg.Listen != "0.0.0.0:3128" || cfg.MetricsListen != wantMetrics || cfg.CADir != "/state/ca" || cfg.Cache.Dir != "/cache/data" {
		t.Fatal("configuration disagrees with deployed ports or volumes")
	}
}

func proxyContainer(t *testing.T, objects map[string]map[string]any) map[string]any {
	t.Helper()
	pod := nested(objects["Deployment/oci-accelerator"], "spec", "template", "spec")
	return named(t, pod["containers"].([]any), "proxy")
}

func deploymentConfig(t *testing.T, objects map[string]map[string]any) config.Config {
	t.Helper()
	data := nested(objects["ConfigMap/oci-accelerator"], "data")["config.yaml"].(string)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func plainDeployment(t *testing.T) map[string]map[string]any {
	t.Helper()
	paths, err := filepath.Glob("../deploy/kubernetes/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatal("plain manifests missing")
	}
	var all bytes.Buffer
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		all.WriteString("---\n")
		all.Write(data)
	}
	return decodeDeployment(t, all.Bytes())
}

func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("OCI_ACCELERATOR_DEPLOYMENT_TESTS") == "1" {
			t.Fatal("Helm is required for deployment checks")
		}
		t.Skip("Helm not installed; CI runs deployment checks with pinned Helm")
	}
}

func renderChart(t *testing.T, extra ...string) map[string]map[string]any {
	t.Helper()
	args := []string{"template", "oci-accelerator", "../deploy/helm/oci-accelerator", "--namespace", "oci-accelerator", "--set", "image.tag=dev"}
	output, err := exec.Command("helm", append(args, extra...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	return decodeDeployment(t, output)
}

func decodeDeployment(t *testing.T, data []byte) map[string]map[string]any {
	t.Helper()
	result := make(map[string]map[string]any)
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err == io.EOF {
			return result
		} else if err != nil {
			t.Fatal(err)
		}
		if len(object) == 0 {
			continue
		}
		key := object["kind"].(string) + "/" + nested(object, "metadata")["name"].(string)
		if _, exists := result[key]; exists {
			t.Fatalf("duplicate resource %s", key)
		}
		result[key] = object
	}
}

func nested(object map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		object, _ = object[key].(map[string]any)
	}
	return object
}

func named(t *testing.T, items []any, name string) map[string]any {
	t.Helper()
	for _, item := range items {
		object := item.(map[string]any)
		if object["name"] == name {
			return object
		}
	}
	t.Fatalf("missing named item %s", name)
	return nil
}
