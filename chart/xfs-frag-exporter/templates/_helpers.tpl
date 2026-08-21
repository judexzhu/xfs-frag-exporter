{{- define "xfs-frag-exporter.name" -}}
xfs-frag-exporter
{{- end -}}

{{- define "xfs-frag-exporter.labels" -}}
app.kubernetes.io/name: xfs-frag-exporter
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: xfs-frag-exporter-{{ .Chart.Version }}
{{- end -}}

{{- define "xfs-frag-exporter.selectorLabels" -}}
app.kubernetes.io/name: xfs-frag-exporter
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
