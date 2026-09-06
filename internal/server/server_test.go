package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/excavador/netbox-mcp/internal/netbox"
)

type capture struct {
	method string
	path   string
	query  url.Values
	body   map[string]any
}

// connect drives the real registered tools over an in-memory transport. The
// bugs worth catching are in how a tool wires itself to the client, so testing
// the helpers directly would prove the wrong thing.
func connect(t *testing.T, got *capture) *mcp.ClientSession {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/":
			_ = json.NewEncoder(w).Encode(map[string]any{"dcim": "x", "ipam": "x", "extras": "x", "core": "x"})
		case "/api/dcim/":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": "x", "sites": "x"})
		case "/api/ipam/":
			_ = json.NewEncoder(w).Encode(map[string]any{"ip-addresses": "x", "prefixes": "x"})
		case "/api/extras/":
			_ = json.NewEncoder(w).Encode(map[string]any{"scripts": "x", "webhooks": "x", "tags": "x"})
		case "/api/core/":
			_ = json.NewEncoder(w).Encode(map[string]any{"object-changes": "x", "data-sources": "x"})
		default:
			if got != nil {
				got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.Query()

				got.body = nil
				_ = json.NewDecoder(r.Body).Decode(&got.body)
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "count": 0, "results": []any{}})
		}
	}))
	t.Cleanup(srv.Close)

	c := netbox.New(srv.URL, "t")

	r := &netbox.Registry{}
	if err := r.Load(context.Background(), c); err != nil {
		t.Fatalf("registry: %v", err)
	}

	ct, st := mcp.NewInMemoryTransports()

	ss, err := New(c, r, "test").Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	return res
}

// Annotations are how a client decides what it may call without asking. An
// unannotated tool defaults to the cautious reading, so a missing one is a
// silent loss of function rather than an error.
func TestEveryToolIsAnnotated(t *testing.T) {
	cs := connect(t, nil)

	tools, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(tools.Tools) < 7 {
		t.Fatalf("got %d tools, want the full set -- did a group fail to register?", len(tools.Tools))
	}

	var reads, writes int

	for _, tool := range tools.Tools {
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s: no annotations", tool.Name)
			continue
		}

		switch {
		case a.ReadOnlyHint:
			reads++
		case a.DestructiveHint != nil:
			writes++
		default:
			t.Errorf("%s: neither read-only nor destructive-hinted", tool.Name)
		}
	}

	if reads == 0 || writes == 0 {
		t.Errorf("reads=%d writes=%d, want both", reads, writes)
	}
}

// The whole point of the write tools is that some object types are not
// writable. If the refusal ever stops working, NetBox will happily accept a
// script -- which it then executes.
func TestDangerousTypesAreNotWritable(t *testing.T) {
	cs := connect(t, nil)

	for _, objectType := range []string{"extras.script", "extras.webhook", "core.datasource"} {
		for _, tool := range []string{"netbox_create_object", "netbox_update_object", "netbox_delete_object"} {
			args := map[string]any{"objectType": objectType, "id": 1, "body": map[string]any{"name": "x"}}

			res := callTool(t, cs, tool, args)
			if !res.IsError {
				t.Errorf("%s accepted %s; it must be refused", tool, objectType)
			}
		}
	}
}

// ...while the ordinary ones stay writable. A boundary that refuses everything
// looks identical to one that works.
func TestOrdinaryTypesAreWritable(t *testing.T) {
	got := &capture{}
	cs := connect(t, got)

	res := callTool(t, cs, "netbox_create_object", map[string]any{
		"objectType": "dcim.device", "body": map[string]any{"name": "sw1"},
	})
	if res.IsError {
		t.Fatalf("create refused: %v", res.Content)
	}

	if got.method != http.MethodPost || got.path != "/api/dcim/devices/" {
		t.Errorf("got %s %s, want POST /api/dcim/devices/", got.method, got.path)
	}

	if got.body["name"] != "sw1" {
		t.Errorf("body = %v, want the object we passed", got.body)
	}
}

// An update must be a PATCH. A PUT would clear every field the caller did not
// mention, which is not what "change some fields" means.
func TestUpdateIsAPatch(t *testing.T) {
	got := &capture{}
	cs := connect(t, got)

	callTool(t, cs, "netbox_update_object", map[string]any{
		"objectType": "dcim.device", "id": 7, "body": map[string]any{"comments": "x"},
	})

	if got.method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH -- a PUT would clear the unmentioned fields", got.method)
	}

	if got.path != "/api/dcim/devices/7/" {
		t.Errorf("path = %s, want /api/dcim/devices/7/", got.path)
	}
}

// A list value repeats the parameter; that is how NetBox expresses OR.
func TestFiltersBecomeQueryParameters(t *testing.T) {
	got := &capture{}
	cs := connect(t, got)

	callTool(t, cs, "netbox_get_objects", map[string]any{
		"objectType": "dcim.device",
		"filters":    map[string]any{"site_id": 3, "id": []any{1, 2}, "name__ic": "sw"},
		"fields":     []any{"id", "name"},
	})

	if got.query.Get("site_id") != "3" {
		t.Errorf("site_id = %q, want 3 -- a JSON number must not render in exponent form", got.query.Get("site_id"))
	}

	if ids := got.query["id"]; len(ids) != 2 {
		t.Errorf("id = %v, want the parameter repeated twice", ids)
	}

	if got.query.Get("name__ic") != "sw" {
		t.Errorf("name__ic = %q", got.query.Get("name__ic"))
	}

	if got.query.Get("fields") != "id,name" {
		t.Errorf("fields = %q, want id,name", got.query.Get("fields"))
	}
}

// NetBox ignores __in and answers with everything, which reads as a filter
// that matched broadly rather than one that was silently dropped.
func TestInSuffixIsRefused(t *testing.T) {
	cs := connect(t, nil)

	res := callTool(t, cs, "netbox_get_objects", map[string]any{
		"objectType": "dcim.device", "filters": map[string]any{"id__in": "1,2"},
	})
	if !res.IsError {
		t.Fatal("__in was accepted; NetBox ignores it and returns everything")
	}

	if !strings.Contains(joinContent(res), "list") {
		t.Errorf("refusal does not say what to do instead: %v", res.Content)
	}
}

// An unbounded list can return a whole estate. The default has to be small and
// the ceiling has to hold.
func TestLimitIsBounded(t *testing.T) {
	got := &capture{}
	cs := connect(t, got)

	callTool(t, cs, "netbox_get_objects", map[string]any{"objectType": "dcim.device"})

	if got.query.Get("limit") != "25" {
		t.Errorf("default limit = %q, want 25", got.query.Get("limit"))
	}

	callTool(t, cs, "netbox_get_objects", map[string]any{"objectType": "dcim.device", "limit": 100000})

	if got.query.Get("limit") != "200" {
		t.Errorf("limit = %q, want it capped at 200", got.query.Get("limit"))
	}
}

func joinContent(res *mcp.CallToolResult) string {
	var b strings.Builder

	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}

	return b.String()
}

// NetBox has no /api/search/ -- that endpoint does not exist, and pointing a
// tool at it produced a 404 with an HTML error page. Search has to be a
// fan-out of the per-type q filter, so what matters is that it queries real
// endpoints and survives one of them failing.
func TestSearchFansOutOverRealEndpoints(t *testing.T) {
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/":
			_ = json.NewEncoder(w).Encode(map[string]any{"dcim": "x", "ipam": "x"})
		case "/api/dcim/":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": "x", "sites": "x"})
		case "/api/ipam/":
			_ = json.NewEncoder(w).Encode(map[string]any{"prefixes": "x"})
		case "/api/dcim/sites/":
			paths = append(paths, r.URL.Path)
			w.WriteHeader(http.StatusForbidden) // one type the token may not read
			_, _ = w.Write([]byte(`{"detail":"nope"}`))
		default:
			paths = append(paths, r.URL.Path)

			if r.URL.Query().Get("q") == "" {
				t.Errorf("%s called without q", r.URL.Path)
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 1, "results": []any{map[string]any{"id": 1, "display": "sw1"}},
			})
		}
	}))
	defer srv.Close()

	c := netbox.New(srv.URL, "t")

	reg := &netbox.Registry{}
	if err := reg.Load(context.Background(), c); err != nil {
		t.Fatalf("registry: %v", err)
	}

	ct, st := mcp.NewInMemoryTransports()

	ss, err := New(c, reg, "test").Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	defer ss.Close()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "netbox_search",
		Arguments: map[string]any{
			"query": "sw1", "objectTypes": []any{"dcim.device", "dcim.site", "ipam.prefix"},
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("search: %v %v", err, res)
	}

	out, _ := res.StructuredContent.(map[string]any)

	matches, _ := out["matches"].(map[string]any)
	if _, ok := matches["dcim.device"]; !ok {
		t.Errorf("no device matches: %v", out)
	}

	if _, ok := matches["ipam.prefix"]; !ok {
		t.Errorf("a forbidden type lost the results from the others: %v", out)
	}

	failed, _ := out["failed"].(map[string]any)
	if _, ok := failed["dcim.site"]; !ok {
		t.Errorf("the failing type was not reported: %v", out)
	}

	for _, p := range paths {
		if p == "/api/search/" {
			t.Error("called /api/search/, which does not exist in NetBox")
		}
	}
}
