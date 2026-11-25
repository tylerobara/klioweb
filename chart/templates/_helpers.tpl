{{- define "klioweb.name" -}}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{- end }}
{{- define "klioweb.labels" -}}
app.kubernetes.io/name: klioweb
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}
{{- define "klioweb.selector" -}}
app.kubernetes.io/name: klioweb
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- define "klioweb.sa" -}}
{{- if .Values.serviceAccount.create }}{{ default (include "klioweb.name" .) .Values.serviceAccount.name }}{{ else }}{{ default "default" .Values.serviceAccount.name }}{{ end }}
{{- end }}
