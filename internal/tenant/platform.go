package tenant

import (
	"text/template"
)

// Pinned platform component versions.
const (
	CNPGChartVersion = "0.29.1"
	RegistryImage    = "registry:3.1.2"
	RailpackVersion  = "v0.43.0"
	RegistryIP       = "10.43.200.200"
	RegistryPullHost = "sb-registry.local:5000"
)

// registriesYAML lets containerd pull images built in the cluster.
const registriesYAML = `mirrors:
  "` + RegistryPullHost + `":
    endpoint:
      - "http://` + RegistryIP + `:5000"
`

// PlatformParams fill the platform manifest.
type PlatformParams struct {
	Domain          string
	ControlImage    string
	ControlReplicas int
}

var platformTmpl = template.Must(template.New("platform").Parse(`apiVersion: v1
kind: Namespace
metadata:
  name: sb-system
---
apiVersion: v1
kind: Namespace
metadata:
  name: sb-builds
  labels:
    pod-security.kubernetes.io/enforce: privileged
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: apps.slicedbread.dev
spec:
  group: slicedbread.dev
  scope: Cluster
  names: {plural: apps, singular: app, kind: App, shortNames: [sbapp]}
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec: {type: object, x-kubernetes-preserve-unknown-fields: true}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: approvals.slicedbread.dev
spec:
  group: slicedbread.dev
  scope: Cluster
  names: {plural: approvals, singular: approval, kind: Approval}
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec: {type: object, x-kubernetes-preserve-unknown-fields: true}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: sb-control
  namespace: sb-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: sb-control
rules:
  - apiGroups: ["", "apps", "batch", "networking.k8s.io", "traefik.io", "postgresql.cnpg.io", "slicedbread.dev", "coordination.k8s.io", "policy"]
    resources: ["*"]
    verbs: ["*"]
  # Certificate status for alerts, and the ZeroSSL fallback issuer it enables after setup.
  - apiGroups: ["cert-manager.io"]
    resources: ["certificates", "clusterissuers"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: sb-control
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: sb-control}
subjects:
  - {kind: ServiceAccount, name: sb-control, namespace: sb-system}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sb-control
  namespace: sb-system
spec:
  # Two replicas on 3-node installs: the login wall in front of every app stays up while a
  # server reboots or dies. State lives in the cluster and the registry, not on a volume.
  replicas: {{.ControlReplicas}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  selector:
    matchLabels: {app: sb-control}
  template:
    metadata:
      labels: {app: sb-control}
    spec:
      serviceAccountName: sb-control
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        fsGroup: 65532
        seccompProfile: {type: RuntimeDefault}
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm: {topologyKey: kubernetes.io/hostname, labelSelector: {matchLabels: {app: sb-control}}}
      tolerations:
        - {key: node.kubernetes.io/not-ready, operator: Exists, effect: NoExecute, tolerationSeconds: 30}
        - {key: node.kubernetes.io/unreachable, operator: Exists, effect: NoExecute, tolerationSeconds: 30}
      volumes:
        - name: data
          emptyDir: {sizeLimit: 1Gi} # cache of uploaded source; the registry holds the copy
      containers:
        - name: sb-control
          image: {{.ControlImage}}
          imagePullPolicy: IfNotPresent
          ports: [{containerPort: 8080}]
          env:
            - {name: SB_DOMAIN, value: "{{.Domain}}"}
            - {name: SB_RAILPACK_VERSION, value: "` + RailpackVersion + `"}
            - {name: SB_REGISTRY_PULL, value: "` + RegistryPullHost + `"}
          volumeMounts: [{name: data, mountPath: /data}]
          readinessProbe:
            httpGet: {path: /healthz, port: 8080}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
            readOnlyRootFilesystem: true
          resources:
            requests: {cpu: 50m, memory: 64Mi}
            limits: {memory: 256Mi}
---
# sb-guard replaces dead servers on 3-node installs. Same binary, no volume, and it leaves
# an unreachable node after 20 seconds, so it is back within about a minute when its own
# server dies.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sb-guard
  namespace: sb-system
spec:
  replicas: {{.ControlReplicas}}
  selector:
    matchLabels: {app: sb-guard}
  template:
    metadata:
      labels: {app: sb-guard}
    spec:
      serviceAccountName: sb-control
      # The server's own resolver: the guard must reach the Hetzner API even while
      # cluster DNS is down with a dead server.
      dnsPolicy: Default
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm: {topologyKey: kubernetes.io/hostname, labelSelector: {matchLabels: {app: sb-guard}}}
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      tolerations:
        - {key: node.kubernetes.io/not-ready, operator: Exists, effect: NoExecute, tolerationSeconds: 20}
        - {key: node.kubernetes.io/unreachable, operator: Exists, effect: NoExecute, tolerationSeconds: 20}
      containers:
        - name: sb-guard
          image: {{.ControlImage}}
          imagePullPolicy: IfNotPresent
          env:
            - {name: SB_MODE, value: guard}
            - name: NODE_NAME
              valueFrom: {fieldRef: {fieldPath: spec.nodeName}}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
            readOnlyRootFilesystem: true
          resources:
            requests: {cpu: 10m, memory: 32Mi}
            limits: {memory: 128Mi}
{{- if gt .ControlReplicas 1}}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: sb-control
  namespace: sb-system
spec:
  minAvailable: 1
  selector:
    matchLabels: {app: sb-control}
{{- end}}
---
apiVersion: v1
kind: Service
metadata:
  name: sb-control
  namespace: sb-system
spec:
  selector: {app: sb-control}
  ports: [{port: 8080, targetPort: 8080}]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: sb-control
  namespace: sb-system
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: websecure
    traefik.ingress.kubernetes.io/router.tls: "true"
spec:
  ingressClassName: traefik
  rules:
    - host: api.{{.Domain}}
      http:
        paths: [{path: /, pathType: Prefix, backend: {service: {name: sb-control, port: {number: 8080}}}}]
    - host: auth.{{.Domain}}
      http:
        paths: [{path: /, pathType: Prefix, backend: {service: {name: sb-control, port: {number: 8080}}}}]
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: sb-registry
  namespace: sb-system
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: hcloud-volumes
  resources:
    requests: {storage: 10Gi}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sb-registry
  namespace: sb-system
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector:
    matchLabels: {app: sb-registry}
  template:
    metadata:
      labels: {app: sb-registry}
    spec:
      securityContext:
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: registry
          image: ` + RegistryImage + `
          ports: [{containerPort: 5000}]
          env:
            # No deletes through the API; sb-control's garbage collection job cleans up on disk.
            - {name: REGISTRY_STORAGE_DELETE_ENABLED, value: "false"}
          volumeMounts: [{name: data, mountPath: /var/lib/registry}]
          readinessProbe:
            httpGet: {path: /v2/, port: 5000}
      volumes:
        - name: data
          persistentVolumeClaim: {claimName: sb-registry}
---
apiVersion: v1
kind: Service
metadata:
  name: sb-registry
  namespace: sb-system
spec:
  clusterIP: ` + RegistryIP + `
  selector: {app: sb-registry}
  ports: [{port: 5000, targetPort: 5000}]
---
# Builds run as one-off Jobs with their own rootless BuildKit; nothing needs to reach them.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-ingress
  namespace: sb-builds
spec:
  podSelector: {}
  policyTypes: [Ingress]
---
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: cloudnative-pg
  namespace: kube-system
spec:
  repo: https://cloudnative-pg.github.io/charts
  chart: cloudnative-pg
  version: ` + CNPGChartVersion + `
  targetNamespace: cnpg-system
  createNamespace: true
`))

// PlatformManifest renders the SlicedBread platform components.
func PlatformManifest(p PlatformParams) (string, error) { return render(platformTmpl, p) }
