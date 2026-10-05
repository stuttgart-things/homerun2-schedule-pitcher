"""
ConfigMap with the SchedulePitcherProfile
"""

import labels

config = labels.config

configMap = {
    apiVersion: "v1"
    kind: "ConfigMap"
    metadata: {
        name: "{}-profile".format(config.name)
        namespace: config.namespace
        labels: labels.commonLabels
    }
    data: {
        "profile.yaml": config.profileYaml
    }
} if config.profileYaml else None
