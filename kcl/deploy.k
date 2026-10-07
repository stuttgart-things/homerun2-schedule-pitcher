"""
Workload: a Deployment for the central instance and for an agent running
`serve`, a CronJob for an agent running `run` once per schedule.
"""

import labels

config = labels.config

_profilePath = "/etc/homerun2-schedule-pitcher/profile.yaml"
_isCronJob = labels.isAgent and config.agentKind == "cronjob"

_secretEnv = lambda name: str, secret: str, key: str -> {str:} {
    {
        name: name
        valueFrom: {
            secretKeyRef: {
                name: "{}-{}".format(config.name, secret)
                key: key
                optional: True
            }
        }
    }
}

_env = [
    {name: "PROFILE_PATH", value: _profilePath}
    {name: "LOG_FORMAT", value: "json"}
    {
        name: "POD_NAMESPACE"
        valueFrom: {fieldRef: {fieldPath: "metadata.namespace"}}
    }
    _secretEnv("AUTH_TOKEN", "token", "auth-token")
    # Named on the login page, where to find the token.
    {name: "AUTH_TOKEN_SECRET", value: "{}-token".format(config.name)}
] + ([
    {name: "REDIS_ADDR", value: config.redisAddr}
    {name: "REDIS_PORT", value: config.redisPort}
    _secretEnv("REDIS_PASSWORD", "redis", "password")
    _secretEnv("PITCHER_TOKEN", "pitcher", "token")
] if labels.isCentral else [
    _secretEnv("REPORT_TOKEN", "report", "token")
]) + ([
    {name: "SSL_CERT_DIR", value: "/etc/ssl/custom:/etc/ssl/certs"}
] if config.trustBundleConfigMap else []) + [
    {name: k, value: v} for k, v in config.extraEnvVars
]

_container = {
    name: config.name
    image: config.image
    imagePullPolicy: config.imagePullPolicy
    args: ["run"] if _isCronJob else ["serve"]
    env: _env
    if not _isCronJob:
        ports: [{name: "http", containerPort: 8080, protocol: "TCP"}]
        readinessProbe: {
            httpGet: {path: "/ready", port: "http"}
            periodSeconds: 10
        }
        livenessProbe: {
            httpGet: {path: "/health", port: "http"}
            periodSeconds: 20
        }
    resources: {
        requests: {cpu: config.cpuRequest, memory: config.memoryRequest}
        limits: {cpu: config.cpuLimit, memory: config.memoryLimit}
    }
    securityContext: {
        readOnlyRootFilesystem: True
        runAsNonRoot: True
        allowPrivilegeEscalation: False
        capabilities: {drop: ["ALL"]}
    }
    volumeMounts: [
        {name: "profile", mountPath: "/etc/homerun2-schedule-pitcher", readOnly: True}
        {name: "tmp", mountPath: "/tmp"}
    ] + ([
        {name: "trust-bundle", mountPath: "/etc/ssl/custom/trust-bundle.pem", subPath: "trust-bundle.pem", readOnly: True}
    ] if config.trustBundleConfigMap else [])
}

_podSpec = {
    serviceAccountName: config.name
    if _isCronJob:
        restartPolicy: "Never"
    containers: [_container]
    securityContext: {
        runAsNonRoot: True
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile: {type: "RuntimeDefault"}
    }
    volumes: [
        {name: "profile", configMap: {name: "{}-profile".format(config.name)}}
        {name: "tmp", emptyDir: {}}
    ] + ([
        {name: "trust-bundle", configMap: {name: config.trustBundleConfigMap, optional: True}}
    ] if config.trustBundleConfigMap else [])
}

deployment = {
    apiVersion: "apps/v1"
    kind: "Deployment"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    spec: {
        replicas: config.replicas
        selector: {matchLabels: labels.selectorLabels}
        template: {
            metadata: {labels: labels.commonLabels}
            spec: _podSpec
        }
    }
} if not _isCronJob else None

cronJob = {
    apiVersion: "batch/v1"
    kind: "CronJob"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    spec: {
        schedule: config.agentSchedule
        concurrencyPolicy: "Forbid"
        successfulJobsHistoryLimit: 1
        failedJobsHistoryLimit: 3
        jobTemplate: {
            spec: {
                backoffLimit: 1
                activeDeadlineSeconds: 600
                template: {
                    metadata: {labels: labels.commonLabels}
                    spec: _podSpec
                }
            }
        }
    }
} if _isCronJob else None
