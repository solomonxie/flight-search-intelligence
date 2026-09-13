{{/*
Include from a consuming chart's own templates/ (a plain include, not a
standalone template file, so the hook Job is namespaced/owned by that
chart's release):

    {{ include "flyway-migrate.job" . }}

Reads .Values.flywayMigrate from the *including* chart:
  enabled           bool    — opt-in per chart, default false
  image             string  — databases/postgres/Dockerfile's built image
  postgresHost      string  — helm/postgres's Service name
  postgresPort      int     — default 5432
  database          string
  credentialsSecret string  — Secret with username/password keys
    (helm/postgres/values.yaml's credentials.secretName)
*/}}
{{- define "flyway-migrate.job" -}}
{{- if .Values.flywayMigrate.enabled -}}
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ .Release.Name }}-flyway-migrate
  annotations:
    "helm.sh/hook": pre-install,pre-upgrade
    "helm.sh/hook-weight": "0"
    "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
spec:
  backoffLimit: 2
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: flyway-migrate
          image: {{ .Values.flywayMigrate.image | quote }}
          env:
            - name: FLYWAY_URL
              value: "jdbc:postgresql://{{ .Values.flywayMigrate.postgresHost }}:{{ .Values.flywayMigrate.postgresPort | default 5432 }}/{{ .Values.flywayMigrate.database }}"
            - name: FLYWAY_USER
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.flywayMigrate.credentialsSecret }}
                  key: username
            - name: FLYWAY_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.flywayMigrate.credentialsSecret }}
                  key: password
{{- end -}}
{{- end -}}
