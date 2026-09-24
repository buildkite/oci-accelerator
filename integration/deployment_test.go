package integration

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/oci-accelerator/internal/config"
	"go.yaml.in/yaml/v3"
)

func TestPlainDeploymentSafety(t *testing.T) {
	checkDeployment(t, plainDeployment(t), false)
}

func TestHelmDeploymentParity(t *testing.T) {
	requireHelm(t)
	plain := plainDeployment(t)
	helm := renderChart(t)
	checkDeployment(t, helm, false)
	if !reflect.DeepEqual(deploymentConfig(t, plain), deploymentConfig(t, helm)) {
		t.Fatal("Helm and plain manifests resolve to different default application configurations")
	}
	for _, kind := range []string{"Deployment", "Service", "NetworkPolicy", "PersistentVolumeClaim"} {
		name := "oci-accelerator"
		if kind == "PersistentVolumeClaim" {
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
	checkDeployment(t, remote, true)
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

func checkDeployment(t *testing.T, objects map[string]map[string]any, remote bool) {
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
	for key := range objects {
		kind, name, _ := strings.Cut(key, "/")
		if slices.Contains([]string{"Job", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding"}, kind) || kind == "ServiceAccount" && strings.Contains(name, "bootstrap") {
			t.Fatalf("deployment package must not contain bootstrap API resource %s", key)
		}
		if kind == "Secret" || kind == "ConfigMap" && strings.HasSuffix(name, "-trust") {
			t.Fatalf("deployment templates must not own operator-provisioned CA resource %s", key)
		}
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
	secret := nested(named(t, pod["volumes"].([]any), "ca-secret"), "secret")
	if secret["secretName"] != "oci-accelerator-ca" || secret["optional"] != nil {
		t.Fatal("serving pod must require the preexisting CA Secret")
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
