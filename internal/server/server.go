// Package server exposes a NetBox instance as MCP tools.
//
// NetBox's REST API is uniform: every model lives at /api/{app}/{endpoint}/
// and answers the same list, detail, create, update and delete verbs. So the
// tools here are generic over an object type rather than one pair per model --
// NetBox serves several hundred models, and a tool per model would bury a
// client in names it will never call.
//
// What is deliberately NOT writable, and why:
//
//   - extras.script          scripts are arbitrary Python that NetBox runs.
//     Writing one is remote code execution.
//   - extras.webhook         and eventrule: both make NetBox issue HTTP
//     requests of the author's choosing, which is a
//     data exfiltration primitive.
//   - core.datasource        syncs config and scripts from a remote repo, so
//     writing one is code execution one step removed.
//   - extras.configtemplate  rendered against real device data and pushed to
//     devices.
//
// The users app is not reachable at all -- not for reading either. Tokens,
// permissions and group membership are credentials, and a server holding one
// token has no business enumerating the others.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/excavador/netbox-mcp/internal/netbox"
)

// defaultLimit keeps a careless list call from returning a whole DCIM estate.
// NetBox's own default is 50; a model paging through 50 objects of forty
// fields each fills a context window with data it did not ask for.
const (
	defaultLimit = 25
	maxLimit     = 200
)

// noWrite lists object types that may be read but never created, changed or
// deleted through a tool. Each one is a way to make NetBox execute something
// or talk to somewhere.
var noWrite = map[string]bool{
	"extras.script":         true,
	"extras.scriptmodule":   true,
	"extras.webhook":        true,
	"extras.eventrule":      true,
	"extras.configtemplate": true,
	"core.datasource":       true,
	"core.datafile":         true,
}

func ptr[T any](v T) *T { return &v }

func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
}

func creates() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: false}
}

func updates() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true}
}

func deletes() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true}
}

// New builds the MCP server and registers every tool.
func New(c *netbox.Client, r *netbox.Registry, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "netbox", Version: version}, nil)

	addDiscovery(s, r)
	addReads(s, c, r)
	addWrites(s, c, r)

	return s
}

func addDiscovery(s *mcp.Server, r *netbox.Registry) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "netbox_object_types",
		Description: "List the object types this NetBox serves, as dotted names such as dcim.device " +
			"or ipam.ipaddress. Discovered from the instance itself, so it matches its version and plugins. " +
			"Call this when unsure what an object type is called.",
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, a struct {
		Prefix string `json:"prefix,omitempty" jsonschema:"only types starting with this, e.g. dcim or ipam."`
	},
	) (*mcp.CallToolResult, any, error) {
		all := r.Types()
		if a.Prefix == "" {
			return nil, map[string]any{"objectTypes": all, "count": len(all)}, nil
		}

		var out []string

		for _, t := range all {
			if strings.HasPrefix(t, strings.ToLower(a.Prefix)) {
				out = append(out, t)
			}
		}

		return nil, map[string]any{"objectTypes": out, "count": len(out)}, nil
	})
}

type listArgs struct {
	ObjectType string         `json:"objectType" jsonschema:"dotted type such as dcim.device, or the endpoint form dcim/devices"`
	Filters    map[string]any `json:"filters,omitempty" jsonschema:"NetBox API filters, e.g. {\"site_id\": 1, \"name__ic\": \"switch\"}. A list value repeats the parameter."`
	Fields     []string       `json:"fields,omitempty" jsonschema:"return only these fields. Strongly preferred: full objects are large and most of it is never read."`
	Brief      bool           `json:"brief,omitempty" jsonschema:"return NetBox's minimal representation of each object"`
	Limit      int            `json:"limit,omitempty" jsonschema:"how many to return; defaults to 25, capped at 200"`
	Offset     int            `json:"offset,omitempty" jsonschema:"how many to skip, for paging"`
	Ordering   string         `json:"ordering,omitempty" jsonschema:"field to sort by; prefix with - to reverse"`
}

func addReads(s *mcp.Server, c *netbox.Client, r *netbox.Registry) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "netbox_get_objects",
		Description: "List or filter objects of one type. Always pass fields to keep the response small, " +
			"and prefer filtering in NetBox over fetching everything and sifting. " +
			"Filters are single-hop only: filter by site_id, not by device__site_id.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a listArgs) (*mcp.CallToolResult, any, error) {
		path, err := r.Path(a.ObjectType)
		if err != nil {
			return nil, nil, err
		}

		q, err := listQuery(a)
		if err != nil {
			return nil, nil, err
		}

		return call(ctx, c, http.MethodGet, path, q, nil)
	})

	type getArgs struct {
		ObjectType string   `json:"objectType" jsonschema:"dotted type such as dcim.device"`
		ID         int      `json:"id" jsonschema:"the numeric id"`
		Fields     []string `json:"fields,omitempty" jsonschema:"return only these fields"`
		Brief      bool     `json:"brief,omitempty" jsonschema:"return the minimal representation"`
	}

	mcp.AddTool(s, &mcp.Tool{
		Name: "netbox_get_object", Description: "One object of a given type, by numeric id.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a getArgs) (*mcp.CallToolResult, any, error) {
		path, err := r.Path(a.ObjectType)
		if err != nil {
			return nil, nil, err
		}

		if a.ID <= 0 {
			return nil, nil, fmt.Errorf("id is required")
		}

		q := url.Values{}
		if len(a.Fields) > 0 {
			q.Set("fields", strings.Join(a.Fields, ","))
		}

		if a.Brief {
			q.Set("brief", "true")
		}

		return call(ctx, c, http.MethodGet, path+strconv.Itoa(a.ID)+"/", q, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "netbox_search",
		Description: "Search several object types at once with NetBox's general q filter. " +
			"Use it to find something when the object type is not known; use netbox_get_objects once it is. " +
			"Searches a common set of types by default -- pass objectTypes to widen or narrow it.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a struct {
		Query       string   `json:"query" jsonschema:"what to search for"`
		ObjectTypes []string `json:"objectTypes,omitempty" jsonschema:"types to search; defaults to the common DCIM, IPAM and tenancy ones"`
		Limit       int      `json:"limit,omitempty" jsonschema:"how many per type; defaults to 25, capped at 200"`
	},
	) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(a.Query) == "" {
			return nil, nil, fmt.Errorf("query is required")
		}

		types := a.ObjectTypes
		if len(types) == 0 {
			types = defaultSearchTypes
		}

		limit := strconv.Itoa(clampLimit(a.Limit))
		found := map[string]any{}
		searched := make([]string, 0, len(types))
		failed := map[string]string{}

		for _, t := range types {
			path, err := r.Path(t)
			if err != nil {
				failed[t] = err.Error()
				continue
			}

			out, err := c.Call(ctx, http.MethodGet, path,
				url.Values{"q": {a.Query}, "limit": {limit}, "brief": {"true"}}, nil)
			if err != nil {
				// One type failing -- a permission, a plugin quirk -- should
				// not lose the results from the others.
				failed[t] = err.Error()
				continue
			}

			searched = append(searched, t)

			page, ok := out.(map[string]any)
			if !ok {
				continue
			}

			if results, ok := page["results"].([]any); ok && len(results) > 0 {
				found[t] = results
			}
		}

		res := map[string]any{"matches": found, "searched": searched}
		if len(failed) > 0 {
			res["failed"] = failed
		}

		return nil, res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "netbox_changelog",
		Description: "NetBox's audit trail: who changed what, when, and the before and after. " +
			"Filter it like any other list, e.g. {\"changed_object_type\": \"dcim.device\"}.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a struct {
		Filters map[string]any `json:"filters,omitempty" jsonschema:"filters on the change records"`
		Limit   int            `json:"limit,omitempty" jsonschema:"how many to return; defaults to 25, capped at 200"`
	},
	) (*mcp.CallToolResult, any, error) {
		q, err := filterQuery(a.Filters)
		if err != nil {
			return nil, nil, err
		}

		q.Set("limit", strconv.Itoa(clampLimit(a.Limit)))

		return call(ctx, c, http.MethodGet, "/core/object-changes/", q, nil)
	})
}

func addWrites(s *mcp.Server, c *netbox.Client, r *netbox.Registry) {
	type createArgs struct {
		ObjectType string         `json:"objectType" jsonschema:"dotted type such as dcim.device"`
		Body       map[string]any `json:"body" jsonschema:"the object to create, in NetBox's own shape. Related objects are given by numeric id, e.g. {\"site\": 1}."`
	}

	mcp.AddTool(s, &mcp.Tool{
		Name: "netbox_create_object",
		Description: "Create one object. Required fields differ per type -- read an existing object of " +
			"the same type first if unsure. NetBox rejects a bad body with a message naming the field.",
		Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a createArgs) (*mcp.CallToolResult, any, error) {
		path, err := writablePath(r, a.ObjectType)
		if err != nil {
			return nil, nil, err
		}

		if len(a.Body) == 0 {
			return nil, nil, fmt.Errorf("body is required")
		}

		return call(ctx, c, http.MethodPost, path, nil, a.Body)
	})

	type updateArgs struct {
		ObjectType string         `json:"objectType" jsonschema:"dotted type such as dcim.device"`
		ID         int            `json:"id" jsonschema:"the numeric id"`
		Body       map[string]any `json:"body" jsonschema:"only the fields to change"`
	}

	mcp.AddTool(s, &mcp.Tool{
		Name: "netbox_update_object",
		Description: "Change SOME fields of one object, leaving the rest alone. This is a PATCH: " +
			"fields you do not mention keep their values.",
		Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a updateArgs) (*mcp.CallToolResult, any, error) {
		path, err := writablePath(r, a.ObjectType)
		if err != nil {
			return nil, nil, err
		}

		if a.ID <= 0 {
			return nil, nil, fmt.Errorf("id is required")
		}

		if len(a.Body) == 0 {
			return nil, nil, fmt.Errorf("body is required")
		}

		return call(ctx, c, http.MethodPatch, path+strconv.Itoa(a.ID)+"/", nil, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "netbox_delete_object",
		Description: "DELETE one object permanently. NetBox cascades: deleting a site takes its racks " +
			"and devices with it, and the changelog records it but cannot undo it.",
		Annotations: deletes(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a struct {
		ObjectType string `json:"objectType" jsonschema:"dotted type such as dcim.device"`
		ID         int    `json:"id" jsonschema:"the numeric id"`
	},
	) (*mcp.CallToolResult, any, error) {
		path, err := writablePath(r, a.ObjectType)
		if err != nil {
			return nil, nil, err
		}

		if a.ID <= 0 {
			return nil, nil, fmt.Errorf("id is required")
		}

		return call(ctx, c, http.MethodDelete, path+strconv.Itoa(a.ID)+"/", nil, nil)
	})
}

// writablePath resolves an object type and refuses the ones whose contents
// NetBox executes or acts on. The refusal names the reason: a caller that hits
// it should understand the boundary, not read it as a bug.
func writablePath(r *netbox.Registry, objectType string) (string, error) {
	path, err := r.Path(objectType)
	if err != nil {
		return "", err
	}

	key := strings.ToLower(strings.TrimSpace(objectType))
	if noWrite[key] {
		return "", fmt.Errorf(
			"%s is read-only here: NetBox executes or acts on these, so writing one is code execution "+
				"or an outbound request, not a change to the inventory", key)
	}

	return path, nil
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	default:
		return n
	}
}

func listQuery(a listArgs) (url.Values, error) {
	q, err := filterQuery(a.Filters)
	if err != nil {
		return nil, err
	}

	q.Set("limit", strconv.Itoa(clampLimit(a.Limit)))

	if a.Offset > 0 {
		q.Set("offset", strconv.Itoa(a.Offset))
	}

	if len(a.Fields) > 0 {
		q.Set("fields", strings.Join(a.Fields, ","))
	}

	if a.Brief {
		q.Set("brief", "true")
	}

	if a.Ordering != "" {
		q.Set("ordering", a.Ordering)
	}

	return q, nil
}

// filterQuery turns the filters object into query parameters.
//
// A list value repeats the parameter, which is how NetBox expresses OR:
// ?id=1&id=2. The __in suffix looks like it should do the same and does not --
// NetBox ignores it and answers with everything, which reads as a filter that
// matched broadly rather than one that was dropped.
func filterQuery(filters map[string]any) (url.Values, error) {
	q := url.Values{}

	for k, v := range filters {
		if strings.HasSuffix(k, "__in") {
			return nil, fmt.Errorf(
				"filter %q: NetBox ignores the __in suffix and returns everything. "+
					"Pass a list instead: {%q: [1, 2]}", k, strings.TrimSuffix(k, "__in"))
		}

		switch t := v.(type) {
		case []any:
			for _, item := range t {
				q.Add(k, scalar(item))
			}
		case nil:
			q.Set(k, "null")
		default:
			q.Set(k, scalar(v))
		}
	}

	return q, nil
}

func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// JSON numbers arrive as float64; ids must not render as "1e+06".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}

		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// call performs the request and returns the decoded JSON as structured
// content. MCP requires that to be an object, so a non-object is wrapped --
// the same reason and the same key NetBox uses for its own paged results.
func call(ctx context.Context, c *netbox.Client, method, path string, q url.Values, body any) (*mcp.CallToolResult, any, error) {
	out, err := c.Call(ctx, method, path, q, body)
	if err != nil {
		return nil, nil, err
	}

	if _, ok := out.(map[string]any); !ok {
		return nil, map[string]any{"results": out}, nil
	}

	return nil, out, nil
}

// defaultSearchTypes is what netbox_search covers when the caller does not say.
//
// NetBox has no cross-type search in its REST API -- /api/search/ does not
// exist, it is a UI and GraphQL feature -- so search here is a fan-out of the
// per-type q filter. That costs one request per type, which is why the default
// set is short and deliberately weighted to what someone actually hunts for by
// name.
var defaultSearchTypes = []string{
	"dcim.device",
	"dcim.site",
	"dcim.rack",
	"dcim.interface",
	"ipam.ipaddress",
	"ipam.prefix",
	"ipam.vlan",
	"tenancy.tenant",
	"virtualization.virtualmachine",
}
