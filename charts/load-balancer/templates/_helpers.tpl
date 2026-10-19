{{/*
Chart name, overridable with nameOverride.
*/}}
{{- define "lb.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Release-qualified name. Truncated to 63 characters because some Kubernetes
name fields are limited to that (and Service names double as DNS labels).
*/}}
{{- define "lb.fullname" -}}
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

{{- define "lb.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "lb.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "lb.labels" -}}
helm.sh/chart: {{ include "lb.chart" . }}
{{ include "lb.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- /*
app.kubernetes.io/component is deliberately NOT part of these labels: the demo
backends would then append their own value to the same mapping key, and a
duplicate YAML key is an error in stricter parsers than the one kubectl happens
to use. Each template sets its own component instead.
*/}}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "lb.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "lb.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The port the process binds to, taken from config.listen so containerPort and
the port in the log cannot drift from what the binary actually opens. Handles
both ":8080" and "8080".
*/}}
{{- define "lb.port" -}}
{{- splitList ":" (.Values.config.listen | default ":8080" | toString) | last -}}
{{- end -}}

{{/*
The backend list as YAML, ready to be inlined into the config file.
*/}}
{{- define "lb.backendsYaml" -}}
{{- if .Values.config.backends -}}
{{ toYaml .Values.config.backends }}
{{- else if .Values.demoBackends.enabled -}}
{{- $root := include "lb.fullname" . -}}
{{- $port := .Values.demoBackends.servicePort -}}
{{- range $i := until (int .Values.demoBackends.count) }}
- http://{{ $root }}-demo-{{ add $i 1 }}:{{ $port }}
{{- end }}
{{- else -}}
{{- fail "no backends: set config.backends, or keep demoBackends.enabled to get demo backends" -}}
{{- end -}}
{{- end -}}

{{/*
How many backends the load balancer ends up with - the length of an explicit
config.backends, or the number of demo backends. Used only to validate
config.weights before the pod crash-loops on it.
*/}}
{{- define "lb.backendCount" -}}
{{- if .Values.config.backends -}}
{{- len .Values.config.backends -}}
{{- else -}}
{{- int .Values.demoBackends.count -}}
{{- end -}}
{{- end -}}

{{/*
The load balancer's config file.

It is a file rather than a list of flags because the file format carries lists
(backends, weights) that -backends/-weights can only express as a
comma-separated string - and because a ConfigMap shows up in `kubectl get cm -o
yaml` as something readable.

Values that are null are left out, so the binary's own defaults apply
(config.Parse merges: explicitly set flags > file > defaults).
*/}}
{{- define "lb.configFile" -}}
{{- $c := .Values.config -}}
{{- $weights := $c.weights | default (list) -}}
{{- if and (ne (len $weights) 0) (ne (len $weights) (include "lb.backendCount" . | int)) -}}
{{- fail (printf "config.weights has %d entries but there are %d backends - the binary rejects a mismatch" (len $weights) (include "lb.backendCount" . | int)) -}}
{{- end -}}
backends:
{{ include "lb.backendsYaml" . | trim | indent 2 }}
{{- with $weights }}
weights:
{{ toYaml . | trim | indent 2 }}
{{- end }}
{{- range $k := list "listen" "algorithm" "health_check_every" "health_check_timeout" "health_check_parallelism" "breaker_threshold" "breaker_cooldown" "log_format" "log_level" "max_body_bytes" "max_concurrent" "read_timeout" "write_timeout" }}
{{- $v := index $c $k }}
{{- if not (kindIs "invalid" $v) }}
{{ $k }}: {{ if kindIs "string" $v }}{{ $v | quote }}{{ else if kindIs "float64" $v }}{{ $v | int64 }}{{ else }}{{ $v }}{{ end }}
{{- end }}
{{- end }}
{{- /* extra covers keys this chart does not render; already-rendered keys are
       omitted, because a duplicate mapping key is a parse error in YAML.

       Numbers from values.yaml arrive as float64 (Helm parses YAML into JSON,
       and JSON numbers are float64), and plain %v would print 1048576 as
       1.048576e+06 - a value the binary's *int64 field then refuses to parse.
       That is what the int64 branch above is for. */}}
{{- with omit (default (dict) $c.extra) "listen" "algorithm" "health_check_every" "health_check_timeout" "health_check_parallelism" "breaker_threshold" "breaker_cooldown" "log_format" "log_level" "max_body_bytes" "max_concurrent" "read_timeout" "write_timeout" "backends" "weights" }}
{{ toYaml . | trim }}
{{- end }}
{{- end -}}
