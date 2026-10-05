"""
Schema for deploying homerun2-schedule-pitcher, as the central instance or as
an agent on a watched cluster.
"""

schema SecretAccess:
    """Secrets the agent may read in one namespace"""
    namespace: str
    # Names for get; empty with discovery means every Secret in the namespace
    secretNames: [str] = []

schema SchedulePitcher:
    """Configuration for a homerun2-schedule-pitcher deployment"""

    # central: scheduler, findings, delivery and API on platform.
    # agent: runs checks on a watched cluster and reports to the central
    # instance (spec.report in the profile).
    mode: "central" | "agent" = "central"

    name: str = "homerun2-schedule-pitcher"
    namespace: str = "homerun2"

    image: str = "ghcr.io/stuttgart-things/homerun2-schedule-pitcher:main"
    imagePullPolicy: str = "IfNotPresent"

    replicas: int = 1
    cpuRequest: str = "50m"
    cpuLimit: str = "500m"
    memoryRequest: str = "64Mi"
    memoryLimit: str = "256Mi"

    # SchedulePitcherProfile, mounted from a ConfigMap
    profileYaml: str = ""

    # Bearer token for /api/* and POST /findings (central) or the agent's API
    authToken: str = ""

    # central: Redis of homerun2 (password in <name>-redis, key password)
    redisAddr: str = "redis-stack.homerun2.svc.cluster.local"
    redisPort: str = "6379"
    redisPassword: str = ""

    # central: token for omni-pitcher (<name>-pitcher, env PITCHER_TOKEN)
    pitcherToken: str = ""

    # agent: token for the central POST /findings (<name>-report, env REPORT_TOKEN)
    reportToken: str = ""
    # agent: CronJob running `run` once per schedule, or a Deployment with `serve`
    agentKind: "cronjob" | "deployment" = "cronjob"
    agentSchedule: str = "0 */6 * * *"

    # agent: Secrets to read. One Role per namespace, bound to this
    # ServiceAccount. secretNames restricts get by name; discoveryNamespaces
    # additionally allow list (and so reading every Secret there).
    secretAccess: [SecretAccess] = []
    discoveryNamespaces: [str] = []

    # central: Service and HTTPRoute (UI, API and the agents' POST /findings)
    servicePort: int = 80
    httpRouteEnabled: bool = False
    httpRouteParentRefName: str = ""
    httpRouteParentRefNamespace: str = ""
    httpRouteHostname: str = ""

    # ConfigMap with trust-bundle.pem (e.g. the vault PKI CA) added to the
    # system roots via SSL_CERT_DIR
    trustBundleConfigMap: str = ""

    extraEnvVars: {str:str} = {}
    labels: {str:str} = {}

    check:
        name != "", "name must not be empty"
        namespace != "", "namespace must not be empty"
        replicas >= 0, "replicas must be >= 0"
        not httpRouteEnabled or httpRouteParentRefName != "", "httpRouteParentRefName is required with httpRouteEnabled"
