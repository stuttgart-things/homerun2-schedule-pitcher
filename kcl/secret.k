"""
Secrets, emitted only when a value is given. In GitOps the values usually
come from SOPS or ESO instead; the names below are what the Deployment reads.
"""

import base64
import labels

config = labels.config

_secret = lambda suffix: str, key: str, value: str -> {str:} {
    {
        apiVersion: "v1"
        kind: "Secret"
        metadata: {
            name: "{}-{}".format(config.name, suffix)
            namespace: config.namespace
            labels: labels.commonLabels
        }
        type: "Opaque"
        data: {
            "${key}": base64.encode(value)
        }
    }
}

secretToken = _secret("token", "auth-token", config.authToken) if config.authToken else None
secretRedis = _secret("redis", "password", config.redisPassword) if labels.isCentral and config.redisPassword else None
secretPitcher = _secret("pitcher", "token", config.pitcherToken) if labels.isCentral and config.pitcherToken else None
secretReport = _secret("report", "token", config.reportToken) if labels.isAgent and config.reportToken else None
