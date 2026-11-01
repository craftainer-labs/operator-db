package app

// The manager's RBAC on the group's own resources, which `make manifests`
// turns into config/rbac/role.yaml. Rules for what the controllers touch
// outside the group (Secrets, Events, Namespaces) are added with the
// controllers that need them.

// +kubebuilder:rbac:groups=db.craftainer.dev,resources=clusterdatabaseservers;databaseservers;databases;databaseroles,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=db.craftainer.dev,resources=clusterdatabaseservers/status;databaseservers/status;databases/status;databaseroles/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=db.craftainer.dev,resources=clusterdatabaseservers/finalizers;databaseservers/finalizers;databases/finalizers;databaseroles/finalizers,verbs=update
