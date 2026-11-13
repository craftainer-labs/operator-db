package app

// The manager's RBAC on the group's own resources, which `make manifests`
// turns into config/rbac/role.yaml. Rules for what the controllers touch
// outside the group (Secrets, Events, Namespaces) are added with the
// controllers that need them.

// +kubebuilder:rbac:groups=db.craftainer.dev,resources=clusterdatabaseservers;databaseservers;databases;databaseroles,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=db.craftainer.dev,resources=clusterdatabaseservers/status;databaseservers/status;databases/status;databaseroles/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=db.craftainer.dev,resources=clusterdatabaseservers/finalizers;databaseservers/finalizers;databases/finalizers;databaseroles/finalizers,verbs=update

// What the controllers touch outside the group. Namespaces: allowedNamespaces
// selectors (server.Resolve). Secrets: the servers' admin credentials and CA
// bundles are read (a DatabaseServer's live in its own namespace, so this is a
// ClusterRole rule, not only a Role in the operator namespace) and the binding
// Secrets are written; the manager's cache of Secrets is restricted to its
// managed-by label (binding.CacheByObject), and reads of unlabelled Secrets go
// to the API server. The server probes (probe.Reconciler) also watch Secrets
// and ConfigMaps, so those need list and watch too (the ConfigMap cache is
// restricted to the managed-by label like the Secret cache). ConfigMaps: CA
// bundles only. Events: the Event recorder.

// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core;events.k8s.io,resources=events,verbs=create;patch
