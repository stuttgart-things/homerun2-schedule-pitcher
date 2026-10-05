"""
Namespaced RBAC for the agent: one Role and RoleBinding per namespace it reads
Secrets in. secretNames restricts get by name; a discovery namespace also
allows list, which in Kubernetes includes reading every Secret there.
"""

import labels

config = labels.config

_named = {a.namespace: a.secretNames for a in config.secretAccess}
_namespaces = sorted({ns: True for ns in [a.namespace for a in config.secretAccess] + config.discoveryNamespaces})

_rules = lambda ns: str -> [{str:}] {
    [
        {
            apiGroups: [""]
            resources: ["secrets"]
            verbs: ["get", "list"]
        }
    ] if ns in config.discoveryNamespaces else [
        {
            apiGroups: [""]
            resources: ["secrets"]
            verbs: ["get"]
            if _named[ns]:
                resourceNames: _named[ns]
        }
    ]
}

roles = [
    {
        apiVersion: "rbac.authorization.k8s.io/v1"
        kind: "Role"
        metadata: {
            name: "{}-secrets".format(config.name)
            namespace: ns
            labels: labels.commonLabels
        }
        rules: _rules(ns)
    } for ns in _namespaces
] if labels.isAgent else []

roleBindings = [
    {
        apiVersion: "rbac.authorization.k8s.io/v1"
        kind: "RoleBinding"
        metadata: {
            name: "{}-secrets".format(config.name)
            namespace: ns
            labels: labels.commonLabels
        }
        roleRef: {
            apiGroup: "rbac.authorization.k8s.io"
            kind: "Role"
            name: "{}-secrets".format(config.name)
        }
        subjects: [
            {
                kind: "ServiceAccount"
                name: config.name
                namespace: config.namespace
            }
        ]
    } for ns in _namespaces
] if labels.isAgent else []
