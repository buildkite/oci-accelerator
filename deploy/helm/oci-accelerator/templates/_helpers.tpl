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
