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

# scheduled-run check: read ScheduledRuns and the instances they create in
# every namespace (central and agent).
_scheduledRunName = "{}-scheduled-runs".format(config.name)

_scheduledRunRoles = [
    {
        apiVersion: "rbac.authorization.k8s.io/v1"
        kind: "ClusterRole"
        metadata: {
            name: _scheduledRunName
            labels: labels.commonLabels
        }
        rules: [
            {
                apiGroups: ["resources.stuttgart-things.com"]
                resources: ["scheduledruns"]
                verbs: ["get", "list"]
            }
        ] + [
            {
                apiGroups: [t.apiGroup]
                resources: t.resources
                verbs: ["get", "list"]
            } for t in config.scheduledRunTargets
        ]
    }
] if config.scheduledRunEnabled else []

_scheduledRunBindings = [
    {
        apiVersion: "rbac.authorization.k8s.io/v1"
        kind: "ClusterRoleBinding"
        metadata: {
            name: _scheduledRunName
            labels: labels.commonLabels
        }
        roleRef: {
            apiGroup: "rbac.authorization.k8s.io"
            kind: "ClusterRole"
            name: _scheduledRunName
        }
        subjects: [
            {
                kind: "ServiceAccount"
                name: config.name
                namespace: config.namespace
            }
        ]
    }
] if config.scheduledRunEnabled else []

# k8s-resource check: read the configured resources in every namespace
# (central and agent).
_k8sResourceName = "{}-k8s-resources".format(config.name)

_k8sResourceRoles = [
    {
        apiVersion: "rbac.authorization.k8s.io/v1"
        kind: "ClusterRole"
        metadata: {
            name: _k8sResourceName
            labels: labels.commonLabels
        }
        rules: [
            {
                apiGroups: [t.apiGroup]
                resources: t.resources
                verbs: ["get", "list"]
            } for t in config.k8sResourceTargets
        ]
    }
] if config.k8sResourceTargets else []

_k8sResourceBindings = [
    {
        apiVersion: "rbac.authorization.k8s.io/v1"
        kind: "ClusterRoleBinding"
        metadata: {
            name: _k8sResourceName
            labels: labels.commonLabels
        }
        roleRef: {
            apiGroup: "rbac.authorization.k8s.io"
            kind: "ClusterRole"
            name: _k8sResourceName
        }
        subjects: [
            {
                kind: "ServiceAccount"
                name: config.name
                namespace: config.namespace
            }
        ]
    }
] if config.k8sResourceTargets else []

clusterRoles = _scheduledRunRoles + _k8sResourceRoles
clusterRoleBindings = _scheduledRunBindings + _k8sResourceBindings
