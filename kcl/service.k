"""
Service and HTTPRoute of the central instance (UI, API and POST /findings)
"""

import labels

config = labels.config

service = {
    apiVersion: "v1"
    kind: "Service"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    spec: {
        type: "ClusterIP"
        ports: [{name: "http", port: config.servicePort, targetPort: "http", protocol: "TCP"}]
        selector: labels.selectorLabels
    }
} if labels.isCentral else None

httproute = {
    apiVersion: "gateway.networking.k8s.io/v1"
    kind: "HTTPRoute"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    spec: {
        # group + kind (+ weight) are admission defaults; rendering them avoids
        # drift on every reconcile (same as the sibling services).
        parentRefs: [
            {
                group: "gateway.networking.k8s.io"
                kind: "Gateway"
                name: config.httpRouteParentRefName
                if config.httpRouteParentRefNamespace:
                    namespace: config.httpRouteParentRefNamespace
            }
        ]
        if config.httpRouteHostname:
            hostnames: [config.httpRouteHostname]
        rules: [
            {
                matches: [{path: {type: "PathPrefix", value: "/"}}]
                backendRefs: [{group: "", kind: "Service", name: config.name, port: config.servicePort, weight: 1}]
            }
        ]
    }
} if labels.isCentral and config.httpRouteEnabled else None
