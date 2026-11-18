{{- define "netbox-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The standard Helm idiom, and not merely cosmetic: naively joining
     release and chart name yields netbox-mcp-netbox-mcp for the obvious
     release name, and a route pointing at "netbox-mcp" then fails with
     BackendNotFound -- which stays invisible behind a gateway that rejects
     unauthenticated requests before it ever routes them. */}}
{{- define "netbox-mcp.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "netbox-mcp.labels" -}}
app.kubernetes.io/name: {{ include "netbox-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "netbox-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "netbox-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "netbox-mcp.proxyImage" -}}
{{- $i := .Values.proxy.image -}}
{{- printf "%s:%s@%s" $i.repository $i.tag $i.digest -}}
{{- end -}}

{{/*
The fixed addresses inside the pod.

  proxy inbound   :<service.port>  the pod's only MCP port
  server MCP      127.0.0.1:8081   loopback, no container port
  server ops      :9090            /healthz; nothing of MCP
*/}}
{{- define "netbox-mcp.mcpListen" -}}127.0.0.1:8081{{- end -}}
{{- define "netbox-mcp.opsPort" -}}9090{{- end -}}

{{/* The port of a URL: the one it names, else from its scheme. */}}
{{- define "netbox-mcp.urlPort" -}}
{{- $u := urlParse . -}}
{{- $p := regexFind ":[0-9]+$" $u.host | trimPrefix ":" -}}
{{- default (ternary "80" "443" (eq $u.scheme "http")) $p -}}
{{- end -}}

{{/* The host (no port) of auth.resourceUrl. */}}
{{- define "netbox-mcp.resourceHost" -}}
{{- regexReplaceAll ":[0-9]+$" (urlParse .Values.auth.resourceUrl).host "" -}}
{{- end -}}

{{/* The path of auth.resourceUrl; validate has already refused one that ends in "/". */}}
{{- define "netbox-mcp.resourcePath" -}}
{{- (urlParse .Values.auth.resourceUrl).path -}}
{{- end -}}

{{/* Refuse to render without the facts the pod cannot work without. A missing
     value here otherwise becomes a CrashLoopBackOff, or a pod that serves
     nothing, whose cause is one layer away. The proxy's tag and digest are
     refused by values.schema.json (their pattern), before this runs. */}}
{{- define "netbox-mcp.validate" -}}
{{- if not .Values.netbox.url -}}
{{- fail "netbox.url is required (base URL WITHOUT /api)" -}}
{{- end -}}
{{- if not .Values.netbox.existingSecret -}}
{{- fail "netbox.existingSecret is required: the API token is never a literal in values" -}}
{{- end -}}
{{- if not .Values.auth.issuerUrl -}}
{{- fail "auth.issuerUrl is required: the resource-proxy sidecar validates every caller's token against this issuer; without it the pod would be an unauthenticated door to NetBox" -}}
{{- end -}}
{{- if not .Values.auth.resourceUrl -}}
{{- fail "auth.resourceUrl is required: it is this server's own external URL, the RFC 8707 audience the proxy requires of every token" -}}
{{- end -}}
{{- range $k, $v := dict "auth.issuerUrl" .Values.auth.issuerUrl "auth.resourceUrl" .Values.auth.resourceUrl -}}
{{- if not (regexMatch "^https://[^/?#]+" $v) -}}
{{- fail (printf "%s %q must be an https URL" $k $v) -}}
{{- end -}}
{{- if or (contains "#" $v) (contains "?" $v) -}}
{{- fail (printf "%s %q must carry no query or fragment" $k $v) -}}
{{- end -}}
{{- end -}}
{{- if hasSuffix "/" .Values.auth.resourceUrl -}}
{{- fail (printf "auth.resourceUrl %q ends in \"/\": the proxy requires `aud` to equal it byte for byte, so a trailing slash is a different audience than the policy's" .Values.auth.resourceUrl) -}}
{{- end -}}
{{- if not (include "netbox-mcp.resourcePath" .) -}}
{{- fail (printf "auth.resourceUrl %q has no path. The proxy maps that path to the server's /mcp and the route matches it as a prefix; a bare host would take the whole host. Use a path such as https://mcp.example.com/netbox." .Values.auth.resourceUrl) -}}
{{- end -}}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.ingressFrom) -}}
{{- fail "networkPolicy.enabled is true but networkPolicy.ingressFrom is empty, so the rendered policy would admit NOBODY to the proxy's port, the gateway included. Name the gateway's peers, or set networkPolicy.enabled=false if the cluster's CNI does not enforce NetworkPolicy." -}}
{{- end -}}
{{- if .Values.httpRoute.enabled -}}
{{- if not .Values.httpRoute.parentRefs -}}
{{- fail "httpRoute.enabled is true but httpRoute.parentRefs is empty, so the route would attach to no Gateway and answer nothing. Name the Gateway or ListenerSet it attaches to." -}}
{{- end -}}
{{- if and .Values.httpRoute.hostnames (not (has (include "netbox-mcp.resourceHost" .) .Values.httpRoute.hostnames)) -}}
{{- fail (printf "the host %q of auth.resourceUrl is not in httpRoute.hostnames (%s); the route would not match the URL clients are told to use." (include "netbox-mcp.resourceHost" .) (join ", " .Values.httpRoute.hostnames)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
