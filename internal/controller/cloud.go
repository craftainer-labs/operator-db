package controller

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
)

// The operator's own condition reasons for cloud servers and claims (ADR 0002
// section 16.1; the library has no constants for them). Messages stay coarse:
// a category, never SDK text, an ARN or a credential.
const (
	// ConditionCloudResolved is the server condition that reports whether the
	// target was found in the cloud. Ready and ServerReachable carry the
	// probe's own, generic reasons.
	ConditionCloudResolved = "CloudResolved"
	// ReasonResolved is CloudResolved=True.
	ReasonResolved = "Resolved"
	// ReasonCredentialsNamespaceForbidden: credentials.secretRef names a Secret
	// outside the operator's namespace.
	ReasonCredentialsNamespaceForbidden = "CredentialsNamespaceForbidden"
	// ReasonCredentialsUnavailable: no workload identity was injected, or the
	// Secret is missing or malformed.
	ReasonCredentialsUnavailable = "CredentialsUnavailable"
	// ReasonTargetNotFound: the named instance or cluster does not exist.
	ReasonTargetNotFound = "TargetNotFound"
	// ReasonEndpointOverrideForbidden: endpointOverride without
	// EGRESS_MODE=allow-private, or on a namespaced server.
	ReasonEndpointOverrideForbidden = "EndpointOverrideForbidden"
	// ReasonCloudError: the cloud API failed in another way.
	ReasonCloudError = "CloudError"
	// ReasonAuthModeUnsupported is the claim reason of an authMode the server
	// cannot serve.
	ReasonAuthModeUnsupported = "AuthModeUnsupported"
)

var (
	// ErrNotDiscovered means a cloud server has no discovered address yet: the
	// probe has not resolved it, or it failed.
	ErrNotDiscovered = errors.New("the cloud server's address has not been discovered yet")
	// ErrAuthModeUnsupported means the claim asks for an authMode this server
	// cannot serve.
	ErrAuthModeUnsupported = errors.New("AuthModeUnsupported: authMode IAM is not supported by this server")
)

// CloudError is a failure to resolve a cloud server. Reason is one of the
// Reason constants; the message is a category.
type CloudError struct {
	Reason string
	msg    string
	// transient says the failure may pass by itself (the API throttled or
	// timed out): the last discovered address is kept through it.
	transient bool
}

// Error implements error.
func (e *CloudError) Error() string { return e.Reason + ": " + e.msg }

func cloudErr(reason, msg string) error { return &CloudError{Reason: reason, msg: msg} }

func transientErr(reason, msg string) error {
	return &CloudError{Reason: reason, msg: msg, transient: true}
}

// RDSAPI is the part of the RDS client the operator uses.
type RDSAPI interface {
	DescribeDBInstances(ctx context.Context, in *rds.DescribeDBInstancesInput, opts ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
	DescribeDBClusters(ctx context.Context, in *rds.DescribeDBClustersInput, opts ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error)
}

// NewRDS builds the RDS client of an aws.Config, whose HTTP client dials
// through the egress policy (cloud.Egress.AWS).
func NewRDS(cfg aws.Config) RDSAPI { return rds.NewFromConfig(cfg) }

// cloudSpec is a server's provider settings, as much of them as discovery
// uses.
type cloudSpec struct {
	scope  cloud.Scope
	block  cloud.AWSClusterBlock
	target dbv1alpha1.AWSTarget
	creds  *cloud.Credentials
	// ns is where a Secret without a namespace of its own lives: the server's.
	ns string
}

// cloudOf returns the provider settings of a server, nil for a self-hosted one.
func cloudOf(srv server.Server) *cloudSpec {
	switch s := srv.(type) {
	case *dbv1alpha1.ClusterDatabaseServer:
		if s.Spec.Provider == nil || s.Spec.Provider.AWS == nil {
			return nil
		}
		a := s.Spec.Provider.AWS
		return &cloudSpec{scope: cloud.ScopeCluster, block: a.AWSClusterBlock, target: a.AWSTarget, creds: s.Spec.Credentials}
	case *dbv1alpha1.DatabaseServer:
		if s.Spec.Provider == nil || s.Spec.Provider.AWS == nil {
			return nil
		}
		a := s.Spec.Provider.AWS
		return &cloudSpec{scope: cloud.ScopeNamespaced, block: cloud.AWSClusterBlock{AWSBlock: a.AWSBlock}, target: a.AWSTarget,
			creds: s.Spec.Credentials.AsCredentials(), ns: s.Namespace}
	}
	return nil
}

// discover reads the data-plane address of a cloud server from the RDS API,
// with the operator's credentials to the cloud (WorkloadIdentity or the
// Secret of credentials.secretRef). Every HTTP connection dials through the
// egress policy. It returns nil for a self-hosted server. The errors are
// *CloudError, a category each.
func (c Config) discover(ctx context.Context, cs *cloudSpec, conn server.Connection) (map[string]string, error) {
	if cs == nil {
		return nil, nil
	}
	if c.RDS == nil {
		return nil, cloudErr(ReasonCloudError, "no RDS client is configured")
	}
	secret, err := c.cloudSecret(ctx, cs)
	if err != nil {
		return nil, err
	}
	e := cloud.Egress{Mode: c.EgressMode, Policy: c.Egress, Proxy: conn.Proxy}
	cfg, err := e.AWS(ctx, cs.scope, cs.block, cs.creds, secret)
	switch {
	case errors.Is(err, cloud.ErrEndpointOverrideForbidden), errors.Is(err, cloud.ErrInvalidEndpoint):
		return nil, cloudErr(ReasonEndpointOverrideForbidden, "endpointOverride is not allowed here")
	case err != nil:
		return nil, cloudErr(ReasonCredentialsUnavailable, "the cloud credentials are unavailable")
	}
	// The client is built for this call: release its idle connections after it.
	if hc, ok := cfg.HTTPClient.(interface{ CloseIdleConnections() }); ok {
		defer hc.CloseIdleConnections()
	}
	return describe(ctx, c.RDS(cfg), cs.target, cfg.Region)
}

// cloudSecret reads the credentials Secret, nil unless credentials.source is
// Secret. A cluster server's Secret must be in the operator's namespace.
func (c Config) cloudSecret(ctx context.Context, cs *cloudSpec) (map[string][]byte, error) {
	if cs.creds == nil || cs.creds.Source != cloud.SourceSecret || cs.creds.SecretRef == nil {
		return nil, nil
	}
	ns := cs.ns
	if cs.scope == cloud.ScopeCluster {
		ns = cs.creds.SecretRef.Namespace
		if ns != c.OperatorNamespace {
			return nil, cloudErr(ReasonCredentialsNamespaceForbidden, "credentials.secretRef names a Secret outside the operator's namespace")
		}
	}
	var s corev1.Secret
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: cs.creds.SecretRef.Name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, cloudErr(ReasonCredentialsUnavailable, "the credentials Secret could not be read")
		}
		return nil, transientErr(ReasonCredentialsUnavailable, "the credentials Secret could not be read")
	}
	return s.Data, nil
}

// describe resolves an RDS instance or Aurora cluster to its address.
func describe(ctx context.Context, api RDSAPI, t dbv1alpha1.AWSTarget, region string) (map[string]string, error) {
	var host, arn, id string
	var port *int32
	if t.DBClusterIdentifier != "" {
		out, err := api.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{DBClusterIdentifier: aws.String(t.DBClusterIdentifier)})
		var nf *rdstypes.DBClusterNotFoundFault
		switch {
		case errors.As(err, &nf) || (err == nil && len(out.DBClusters) == 0):
			return nil, cloudErr(ReasonTargetNotFound, "the DB cluster does not exist")
		case err != nil:
			return nil, transientErr(ReasonCloudError, "the RDS API call failed")
		}
		cl := out.DBClusters[0]
		host, port, arn, id = aws.ToString(cl.Endpoint), cl.Port, aws.ToString(cl.DBClusterArn), aws.ToString(cl.DbClusterResourceId)
	} else {
		out, err := api.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(t.DBInstanceIdentifier)})
		var nf *rdstypes.DBInstanceNotFoundFault
		switch {
		case errors.As(err, &nf) || (err == nil && len(out.DBInstances) == 0):
			return nil, cloudErr(ReasonTargetNotFound, "the DB instance does not exist")
		case err != nil:
			return nil, transientErr(ReasonCloudError, "the RDS API call failed")
		}
		in := out.DBInstances[0]
		if in.Endpoint != nil {
			host, port = aws.ToString(in.Endpoint.Address), in.Endpoint.Port
		}
		arn, id = aws.ToString(in.DBInstanceArn), aws.ToString(in.DbiResourceId)
	}
	if host == "" {
		return nil, cloudErr(ReasonCloudError, "the database has no endpoint yet")
	}
	if !validEndpoint(host) {
		return nil, cloudErr(ReasonCloudError, "the database endpoint is not a host name")
	}
	d := map[string]string{dbv1alpha1.DiscoveredEndpoint: host, dbv1alpha1.DiscoveredRegion: region}
	if port != nil {
		d[dbv1alpha1.DiscoveredPort] = strconv.Itoa(int(*port))
	}
	if id != "" {
		d[dbv1alpha1.DiscoveredResourceID] = id
	}
	// arn:partition:rds:region:account:...
	if parts := strings.Split(arn, ":"); len(parts) > 4 && parts[4] != "" {
		d[dbv1alpha1.DiscoveredAccountID] = parts[4]
	}
	return d, nil
}

// validEndpoint says whether a host name read from the cloud is a plain DNS
// name (labels of 1 to 63 letters, digits and inner hyphens), not an IP
// literal: the admin password is sent to it, so nothing else is accepted.
func validEndpoint(h string) bool {
	if len(h) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// serverStatusOf returns the status of a server kind of this operator.
func serverStatusOf(srv server.Server) *dbv1alpha1.DatabaseServerStatus {
	switch s := srv.(type) {
	case *dbv1alpha1.ClusterDatabaseServer:
		return &s.Status
	case *dbv1alpha1.DatabaseServer:
		return &s.Status
	}
	return nil
}

// record writes the outcome of discovery to the server's status: status.discovered
// and the CloudResolved condition. A self-hosted server (cs nil) has neither.
func record(srv server.Server, cs *cloudSpec, disc map[string]string, err error) {
	st := serverStatusOf(srv)
	if st == nil {
		return
	}
	if cs == nil {
		st.Discovered = nil
		meta.RemoveStatusCondition(&st.Conditions, ConditionCloudResolved)
		return
	}
	prev := meta.FindStatusCondition(st.Conditions, ConditionCloudResolved)
	current := prev != nil && prev.ObservedGeneration == srv.GetGeneration()
	cond := metav1.Condition{Type: ConditionCloudResolved, Status: metav1.ConditionTrue, Reason: ReasonResolved,
		Message: "the target was found in the cloud", ObservedGeneration: srv.GetGeneration()}
	if err != nil {
		var ce *CloudError
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, ReasonCloudError, "the cloud server could not be resolved"
		if errors.As(err, &ce) {
			cond.Reason, cond.Message = ce.Reason, ce.msg
		} else {
			ce = nil
		}
		// The last address stays through a transient failure (throttling, a
		// timeout) while the spec is the one it was read for: claims keep
		// working. Anything else, or a changed spec, drops it.
		if ce == nil || !ce.transient || !current {
			st.Discovered = nil
		}
	} else {
		st.Discovered = disc
	}
	meta.SetStatusCondition(&st.Conditions, cond)
}

// regionOf is the AWS region of a cloud server's spec, "" for a self-hosted one.
func regionOf(cs *cloudSpec) string {
	if cs == nil {
		return ""
	}
	return cs.block.Region
}
