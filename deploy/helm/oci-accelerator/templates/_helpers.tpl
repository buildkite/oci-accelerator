{{- define "oci-accelerator.labels" -}}
app.kubernetes.io/name: oci-accelerator
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{- define "oci-accelerator.image" -}}
{{- if .Values.image.digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.image.digest) -}}
{{- fail "image.digest must be a sha256 digest (sha256:<64 hex characters>)" -}}
{{- end -}}
{{ printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else if .Values.image.tag -}}
{{ printf "%s:%s" .Values.image.repository .Values.image.tag }}
{{- else -}}
{{- fail "set image.digest (preferred) or image.tag" -}}
{{- end -}}
{{- end }}

{{- define "oci-accelerator.cacheClaim" -}}
{{- default (printf "%s-cache" .Release.Name) .Values.cache.existingClaim -}}
{{- end }}

{{- define "oci-accelerator.bootstrapJobName" -}}
{{- $name := printf "%s-bootstrap-%v" .Release.Name .Release.Revision -}}
{{- if le (len $name) 63 -}}
{{ $name }}
{{- else -}}
{{ printf "%s-%s-bootstrap-%v" (.Release.Name | trunc 32 | trimSuffix "-") (.Release.Name | sha256sum | trunc 8) .Release.Revision }}
{{- end -}}
{{- end }}
