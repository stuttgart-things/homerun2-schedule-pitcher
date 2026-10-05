"""
Configuration from options and common labels
"""

import schema

_opts = {
    mode = option("config.mode")
    name = option("config.name")
    namespace = option("config.namespace")
    image = option("config.image")
    replicas = option("config.replicas")
    profileYaml = option("config.profileYaml")
    authToken = option("config.authToken")
    redisAddr = option("config.redisAddr")
    redisPort = option("config.redisPort")
    redisPassword = option("config.redisPassword")
    pitcherToken = option("config.pitcherToken")
    reportToken = option("config.reportToken")
    agentKind = option("config.agentKind")
    agentSchedule = option("config.agentSchedule")
    secretAccess = option("config.secretAccess")
    discoveryNamespaces = option("config.discoveryNamespaces")
    httpRouteEnabled = option("config.httpRouteEnabled")
    httpRouteParentRefName = option("config.httpRouteParentRefName")
    httpRouteParentRefNamespace = option("config.httpRouteParentRefNamespace")
    httpRouteHostname = option("config.httpRouteHostname")
    trustBundleConfigMap = option("config.trustBundleConfigMap")
    extraEnvVars = option("config.extraEnvVars")
}

config: schema.SchedulePitcher = schema.SchedulePitcher {
    if _opts.mode:
        mode = _opts.mode
    if _opts.name:
        name = _opts.name
    if _opts.namespace:
        namespace = _opts.namespace
    if _opts.image:
        image = _opts.image
    if _opts.replicas:
        replicas = int(_opts.replicas)
    if _opts.profileYaml:
        profileYaml = _opts.profileYaml
    if _opts.authToken:
        authToken = _opts.authToken
    if _opts.redisAddr:
        redisAddr = _opts.redisAddr
    if _opts.redisPort:
        redisPort = str(_opts.redisPort)
    if _opts.redisPassword:
        redisPassword = _opts.redisPassword
    if _opts.pitcherToken:
        pitcherToken = _opts.pitcherToken
    if _opts.reportToken:
        reportToken = _opts.reportToken
    if _opts.agentKind:
        agentKind = _opts.agentKind
    if _opts.agentSchedule:
        agentSchedule = _opts.agentSchedule
    if _opts.secretAccess:
        secretAccess = [schema.SecretAccess {
            namespace = a.namespace
            secretNames = a.secretNames or []
        } for a in _opts.secretAccess]
    if _opts.discoveryNamespaces:
        discoveryNamespaces = _opts.discoveryNamespaces
    if _opts.httpRouteEnabled:
        httpRouteEnabled = _opts.httpRouteEnabled in [True, "True", "true"]
    if _opts.httpRouteParentRefName:
        httpRouteParentRefName = _opts.httpRouteParentRefName
    if _opts.httpRouteParentRefNamespace:
        httpRouteParentRefNamespace = _opts.httpRouteParentRefNamespace
    if _opts.httpRouteHostname:
        httpRouteHostname = _opts.httpRouteHostname
    if _opts.trustBundleConfigMap:
        trustBundleConfigMap = _opts.trustBundleConfigMap
    if _opts.extraEnvVars:
        extraEnvVars = _opts.extraEnvVars
}

isCentral = config.mode == "central"
isAgent = config.mode == "agent"

commonLabels = {
    "app.kubernetes.io/name": config.name
    "app.kubernetes.io/instance": config.name
    "app.kubernetes.io/component": "scheduler" if isCentral else "agent"
    "app.kubernetes.io/part-of": "homerun2"
    **config.labels
}

selectorLabels = {
    "app.kubernetes.io/name": config.name
    "app.kubernetes.io/instance": config.name
}
