"""
homerun2-schedule-pitcher: central instance or cluster agent.
Namespaces are not emitted: the consuming Application creates them
(CreateNamespace=true), like for the sibling services.
"""

import serviceaccount
import configmap
import secret
import rbac
import deploy
import service

manifests = [
    m for m in [
        serviceaccount.serviceAccount
        configmap.configMap
        secret.secretToken
        secret.secretRedis
        secret.secretPitcher
        secret.secretReport
        deploy.deployment
        deploy.cronJob
        service.service
        service.httproute
    ] if m
] + rbac.roles + rbac.roleBindings
