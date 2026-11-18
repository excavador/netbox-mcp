// Package chart_test holds the chart's pod to the shape its security claim
// rests on: the resource-proxy sidecar is the only container that accepts MCP
// from outside the pod, netbox-mcp listens on loopback with no container port
// for it, and the Service reaches the sidecar's port alone. It renders the
// chart with helm, so it is skipped where helm is not installed (CI runs it
// under devbox, where it is).
package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const base = `
netbox:
  url: http://netbox.example
  existingSecret: token
auth:
  issuerUrl: https://issuer.example.com
  resourceUrl: https://mcp.example.com/netbox
proxy:
  image:
    tag: 1.0.0
    digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
networkPolicy:
  ingressFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: gateway
      podSelector:
        matchLabels:
          app.kubernetes.io/name: envoy
`

const route = `
httpRoute:
  enabled: true
  parentRefs:
    - {group: gateway.networking.k8s.io, kind: Gateway, name: mcp, namespace: gateway, sectionName: https}
  hostnames: [mcp.example.com]
`

type object = map[string]any

func render(t *testing.T, values ...string) (string, error) {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	dir := t.TempDir()
	args := []string{"template", "x", ".", "--namespace", "ns"}

	for i, v := range append([]string{base}, values...) {
		f := filepath.Join(dir, string(rune('a'+i))+".yaml")
		if err := os.WriteFile(f, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}

		args = append(args, "-f", f)
	}

	out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()

	return string(out), err
}

// objects parses the rendered documents by kind.
func objects(t *testing.T, rendered string) map[string]object {
	t.Helper()

	out := map[string]object{}

	for _, d := range strings.Split(rendered, "\n---") {
		var m object
		if yaml.Unmarshal([]byte(d), &m) != nil || m["kind"] == nil {
			continue
		}

		out[m["kind"].(string)] = m
	}

	return out
}

func at(t *testing.T, v any, path ...any) any {
	t.Helper()

	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(object)
			if !ok {
				t.Fatalf("at %v: %v is not an object", path, v)
			}

			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("at %v: no element %d in %v", path, k, v)
			}

			v = l[k]
		}
	}

	return v
}

func containers(t *testing.T, deploy object) map[string]object {
	t.Helper()

	out := map[string]object{}

	for _, c := range at(t, deploy, "spec", "template", "spec", "containers").([]any) {
		out[c.(object)["name"].(string)] = c.(object)
	}

	return out
}

func envOf(c object) map[string]string {
	out := map[string]string{}

	for _, e := range c["env"].([]any) {
		m := e.(object)
		if v, ok := m["value"].(string); ok {
			out[m["name"].(string)] = v
		}
	}

	return out
}

func ports(c object) map[string]any {
	out := map[string]any{}

	for _, p := range c["ports"].([]any) {
		m := p.(object)
		out[m["name"].(string)] = m["containerPort"]
	}

	return out
}

// The sidecar is the pod's only MCP port, the server is on loopback with no
// container port for MCP, and the Service reaches the sidecar's port alone.
func TestSidecarIsTheOnlyListener(t *testing.T) {
	out, err := render(t)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}

	objs := objects(t, out)

	cs := containers(t, objs["Deployment"])
	if len(cs) != 2 {
		t.Fatalf("want 2 containers, got %d", len(cs))
	}

	server, proxy := cs["netbox-mcp"], cs["proxy"]

	// The server: loopback MCP, ops port only, no auth settings.
	env := envOf(server)
	if env["ADDR"] != "127.0.0.1:8081" {
		t.Errorf("server ADDR = %q, want loopback 127.0.0.1:8081", env["ADDR"])
	}

	for _, k := range []string{"ISSUER_URL", "RESOURCE_URL", "SCOPE", "ALLOW_NON_LOOPBACK"} {
		if _, ok := env[k]; ok {
			t.Errorf("server carries %s: auth belongs to the proxy, and the loopback guard must stay on", k)
		}
	}

	if got := ports(server); len(got) != 1 || got["ops"] != 9090 {
		t.Errorf("server ports = %v, want only ops 9090", got)
	}

	// The proxy: the one MCP port, upstream on loopback, the URLs it checks.
	if got := ports(proxy); len(got) != 1 || got["http"] != 8080 {
		t.Errorf("proxy ports = %v, want only http 8080", got)
	}

	penv := envOf(proxy)
	for k, want := range map[string]string{
		"LISTEN":       ":8080",
		"UPSTREAM":     "http://127.0.0.1:8081/mcp",
		"ISSUER_URL":   "https://issuer.example.com",
		"RESOURCE_URL": "https://mcp.example.com/netbox",
		"SCOPE":        "openid",
	} {
		if penv[k] != want {
			t.Errorf("proxy %s = %q, want %q", k, penv[k], want)
		}
	}

	if got := proxy["image"]; got != "ghcr.io/truvity/sluis/resource-proxy:1.0.0@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("proxy image = %v, want repository:tag@digest", got)
	}

	// Pod readiness is the proxy's /readyz.
	if got := at(t, proxy, "readinessProbe", "httpGet", "path"); got != "/readyz" {
		t.Errorf("proxy readiness probe path = %v, want /readyz", got)
	}

	// PSA restricted, and no API token in the pod.
	spec := at(t, objs["Deployment"], "spec", "template", "spec").(object)
	if spec["automountServiceAccountToken"] != false {
		t.Errorf("automountServiceAccountToken = %v, want false", spec["automountServiceAccountToken"])
	}

	if at(t, spec, "securityContext", "runAsNonRoot") != true {
		t.Error("pod is not runAsNonRoot")
	}

	for name, c := range cs {
		sc := at(t, c, "securityContext").(object)
		if sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true {
			t.Errorf("container %s securityContext = %v", name, sc)
		}

		if at(t, sc, "capabilities", "drop", 0) != "ALL" {
			t.Errorf("container %s does not drop ALL capabilities", name)
		}
	}

	// The Service: the proxy's port and no other.
	svc := at(t, objs["Service"], "spec", "ports").([]any)
	if len(svc) != 1 || at(t, svc, 0, "port") != 8080 || at(t, svc, 0, "targetPort") != "http" {
		t.Errorf("Service ports = %v, want the one port 8080 -> http", svc)
	}
}

// Without httpRoute.enabled the chart renders no route; the NetworkPolicy
// admits the named peers on the proxy's port only.
func TestNetworkPolicy(t *testing.T) {
	out, err := render(t)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}

	objs := objects(t, out)
	if _, ok := objs["HTTPRoute"]; ok {
		t.Error("an HTTPRoute is rendered with httpRoute.enabled unset")
	}

	np, ok := objs["NetworkPolicy"]
	if !ok {
		t.Fatal("no NetworkPolicy rendered")
	}

	ing := at(t, np, "spec", "ingress").([]any)
	if len(ing) != 1 {
		t.Fatalf("want one ingress rule, got %d", len(ing))
	}

	if at(t, ing, 0, "ports", 0, "port") != 8080 || len(at(t, ing, 0, "ports").([]any)) != 1 {
		t.Errorf("ingress ports = %v, want 8080 only", at(t, ing, 0, "ports"))
	}

	// One peer carrying both selectors, verbatim.
	peer := at(t, ing, 0, "from", 0).(object)
	if peer["namespaceSelector"] == nil || peer["podSelector"] == nil {
		t.Errorf("ingress peer = %v, want namespaceSelector and podSelector", peer)
	}

	// Egress: DNS (udp+tcp 53), HTTPS to the issuer (443), NetBox (port from
	// netbox.url: 80).
	var egress []int

	for _, r := range at(t, np, "spec", "egress").([]any) {
		for _, p := range r.(object)["ports"].([]any) {
			egress = append(egress, p.(object)["port"].(int))
		}
	}

	if !slices.Equal(egress, []int{53, 53, 443, 80}) {
		t.Errorf("egress ports = %v, want [53 53 443 80]", egress)
	}
}

// The route: the resource path as a prefix, the PRM in both forms, no
// rewrite of the resource, and no timeouts (the gateway's default stays).
func TestHTTPRoute(t *testing.T) {
	out, err := render(t, route)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}

	r := objects(t, out)["HTTPRoute"]
	if r == nil {
		t.Fatal("no HTTPRoute rendered")
	}

	rules := at(t, r, "spec", "rules").([]any)
	if len(rules) != 3 {
		t.Fatalf("want 3 rules, got %d", len(rules))
	}

	for i, want := range []struct{ typ, path string }{
		{"PathPrefix", "/netbox"},
		{"Exact", "/.well-known/oauth-protected-resource/netbox"},
		{"Exact", "/netbox/.well-known/oauth-protected-resource"},
	} {
		if got := at(t, rules, i, "matches", 0, "path", "type"); got != want.typ {
			t.Errorf("rule %d type = %v, want %s", i, got, want.typ)
		}

		if got := at(t, rules, i, "matches", 0, "path", "value"); got != want.path {
			t.Errorf("rule %d path = %v, want %s", i, got, want.path)
		}

		if rules[i].(object)["timeouts"] != nil {
			t.Errorf("rule %d sets timeouts; the route keeps the gateway's default", i)
		}

		if at(t, rules, i, "backendRefs", 0, "port") != 8080 {
			t.Errorf("rule %d does not go to the proxy's port", i)
		}
	}

	// Only the suffix alias rewrites, and to the PRM.
	if rules[0].(object)["filters"] != nil || rules[1].(object)["filters"] != nil {
		t.Error("the resource and PRM rules rewrite: the proxy maps the path itself")
	}

	if got := at(t, rules, 2, "filters", 0, "urlRewrite", "path", "replaceFullPath"); got != "/.well-known/oauth-protected-resource/netbox" {
		t.Errorf("alias rewrites to %v", got)
	}
}

// Every refusal fires: a value that is empty or wrong by default renders
// cleanly and fails later, somewhere else.
func TestRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		values []string
		want   string
	}{
		"no issuer":          {[]string{"auth: {issuerUrl: ''}"}, "/auth/issuerUrl"},
		"no resource":        {[]string{"auth: {resourceUrl: ''}"}, "/auth/resourceUrl"},
		"trailing slash":     {[]string{"auth: {resourceUrl: https://mcp.example.com/netbox/}"}, "ends in"},
		"plain http":         {[]string{"auth: {resourceUrl: http://mcp.example.com/netbox}"}, "https"},
		"no path":            {[]string{"auth: {resourceUrl: https://mcp.example.com}"}, "has no path"},
		"no proxy tag":       {[]string{"proxy: {image: {tag: ''}}"}, "/proxy/image/tag"},
		"no proxy digest":    {[]string{"proxy: {image: {digest: ''}}"}, "/proxy/image/digest"},
		"short proxy digest": {[]string{"proxy: {image: {digest: 'sha256:abc'}}"}, "/proxy/image/digest"},
		"empty ingress":      {[]string{"networkPolicy: {ingressFrom: []}"}, "ingressFrom is empty"},
		"route no parent":    {[]string{"httpRoute: {enabled: true}"}, "parentRefs is empty"},
		"route wrong host":   {[]string{route, "httpRoute: {hostnames: [other.example]}"}, "not in httpRoute.hostnames"},
	} {
		out, err := render(t, tc.values...)
		if err == nil {
			t.Errorf("%s: rendered, want a refusal", name)

			continue
		}

		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: refused with %q, want a message containing %q", name, out, tc.want)
		}
	}
}
