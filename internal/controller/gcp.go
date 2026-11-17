package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sqladmin/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
)

// cloudSQLPort is the port every Cloud SQL for PostgreSQL instance serves.
const cloudSQLPort = "5432"

// Cloud SQL user types of the Admin API (users.insert).
const (
	userTypeIAMUser           = "CLOUD_IAM_USER"
	userTypeIAMServiceAccount = "CLOUD_IAM_SERVICE_ACCOUNT"
)

// The Cloud SQL Admin API answers a users call with a long-running operation.
// The operator waits for it, so the marker of a created user is written in the
// same call (ADR 0005); the interval and the bound are variables for tests.
var (
	sqlAdminPoll = time.Second
	sqlAdminWait = 2 * time.Minute
)

// ErrIAMPrincipalInvalid means iam.principal is not a Cloud SQL user name.
var ErrIAMPrincipalInvalid = errors.New("IAMPrincipalRequired: iam.principal must be the e-mail of a Google account, or of a service account " +
	"without \".gserviceaccount.com\" (name@project.iam), at most 63 characters, in lower case")

// ReasonIAMPrincipalRequired is the claim reason of a missing, refused or
// malformed iam.principal (docs/crd-drafts.md, "CloudAuth").
const ReasonIAMPrincipalRequired = "IAMPrincipalRequired"

var principalPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._%+-]*@[a-z0-9][a-z0-9.-]*[a-z0-9]$`)

// ValidPrincipal says whether p can name a Cloud SQL IAM database user: the
// user's e-mail, or the service account's without ".gserviceaccount.com". It is
// also a PostgreSQL role name, so it is at most 63 bytes (NAMEDATALEN-1).
func ValidPrincipal(p string) bool {
	return len(p) <= 63 && principalPattern.MatchString(p) && !strings.HasSuffix(p, ".gserviceaccount.com")
}

// principalType is the Cloud SQL user type of a valid principal: a service
// account's name ends in ".iam" (project.iam), a user's is a plain e-mail.
func principalType(p string) string {
	if strings.HasSuffix(p, ".iam") {
		return userTypeIAMServiceAccount
	}
	return userTypeIAMUser
}

// SQLAdminAPI is the part of the Cloud SQL Admin API the operator uses.
type SQLAdminAPI interface {
	GetInstance(ctx context.Context, project, instance string) (*sqladmin.DatabaseInstance, error)
	InsertUser(ctx context.Context, project, instance string, u *sqladmin.User) (*sqladmin.Operation, error)
	DeleteUser(ctx context.Context, project, instance, name string) (*sqladmin.Operation, error)
	GetOperation(ctx context.Context, project, name string) (*sqladmin.Operation, error)
}

type sqlAdmin struct{ svc *sqladmin.Service }

// NewSQLAdmin builds the Cloud SQL Admin client of a GCP client: its HTTP
// client adds the access token and dials through the egress policy
// (cloud.Egress.GCP), and its endpoint is the endpointOverride, if any.
func NewSQLAdmin(ctx context.Context, c *cloud.GCPClient) (SQLAdminAPI, error) {
	return newSQLAdmin(ctx, c)
}

// newSQLAdmin is NewSQLAdmin with more client options; a test adds one the SDK
// refuses.
func newSQLAdmin(ctx context.Context, c *cloud.GCPClient, extra ...option.ClientOption) (SQLAdminAPI, error) {
	opts := append([]option.ClientOption{option.WithHTTPClient(c.HTTPClient)}, extra...)
	if c.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(c.Endpoint))
	}
	svc, err := sqladmin.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return sqlAdmin{svc}, nil
}

func (a sqlAdmin) GetInstance(ctx context.Context, project, instance string) (*sqladmin.DatabaseInstance, error) {
	return a.svc.Instances.Get(project, instance).Context(ctx).Do()
}

func (a sqlAdmin) InsertUser(ctx context.Context, project, instance string, u *sqladmin.User) (*sqladmin.Operation, error) {
	return a.svc.Users.Insert(project, instance, u).Context(ctx).Do()
}

func (a sqlAdmin) DeleteUser(ctx context.Context, project, instance, name string) (*sqladmin.Operation, error) {
	return a.svc.Users.Delete(project, instance).Name(name).Context(ctx).Do()
}

func (a sqlAdmin) GetOperation(ctx context.Context, project, name string) (*sqladmin.Operation, error) {
	return a.svc.Operations.Get(project, name).Context(ctx).Do()
}

// httpStatus is the HTTP status of a failed Admin API call, 0 for another error.
func httpStatus(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// sqlAdminErr classifies a failed Cloud SQL Admin API call: throttling, timeouts
// and 5xx answers are transient (the last address is kept), everything else
// (access denied, a bad request) is not. The SDK's text is never kept: it names
// the project, the instance and the principal.
func sqlAdminErr(err error) error {
	var ne net.Error
	st := httpStatus(err)
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) ||
		st == http.StatusTooManyRequests || st == http.StatusRequestTimeout || st >= 500 {
		return transientErr(ReasonCloudError, "the Cloud SQL Admin API call failed")
	}
	return cloudErr(ReasonCloudError, "the Cloud SQL Admin API call failed")
}

// gcpAPI builds the Cloud SQL Admin client of a GCP server with the operator's
// credentials to the cloud, through the egress policy and the server's proxy.
// done releases the client's idle connections.
func (c Config) gcpAPI(ctx context.Context, cs *cloudSpec, conn server.Connection) (api SQLAdminAPI, done func(), err error) {
	secret, err := c.cloudSecret(ctx, cs)
	if err != nil {
		return nil, nil, err
	}
	e := cloud.Egress{Mode: c.EgressMode, Policy: c.Egress, Proxy: conn.Proxy, RootCAs: c.CloudRootCAs}
	gc, err := e.GCP(ctx, cs.scope, cs.gcp, cs.creds, secret)
	if err != nil {
		return nil, nil, egressErr(err)
	}
	if c.SQLAdmin == nil {
		return nil, nil, cloudErr(ReasonCloudError, "no Cloud SQL Admin client is configured")
	}
	api, err = c.SQLAdmin(ctx, gc)
	if err != nil {
		return nil, nil, cloudErr(ReasonCloudError, "the Cloud SQL Admin client could not be built")
	}
	return api, gc.HTTPClient.CloseIdleConnections, nil
}

// connectionName is project:location:instance, the Cloud SQL instance
// connection name.
func (cs *cloudSpec) connectionName() string {
	return cs.gcp.Project + ":" + cs.gcp.Location + ":" + cs.gcpTarget.Instance
}

// discoverGCP resolves a Cloud SQL instance to its address with instances.get.
func (c Config) discoverGCP(ctx context.Context, cs *cloudSpec, conn server.Connection) (map[string]string, error) {
	api, done, err := c.gcpAPI(ctx, cs, conn)
	if err != nil {
		return nil, err
	}
	defer done()
	return describeCloudSQL(ctx, api, cs, c.EgressMode == egress.ModeAllowPrivate)
}

// describeCloudSQL reads the instance's connection name, address, region and DNS
// name. The connection name the API reports must be the one the spec derives
// (project:location:instance): the instance is looked up by project and name
// only, so this is what checks the location. The address is the private IP when
// the operator may dial private addresses and the instance has one, else the
// public one (the egress policy still decides at dial time).
func describeCloudSQL(ctx context.Context, api SQLAdminAPI, cs *cloudSpec, allowPrivate bool) (map[string]string, error) {
	in, err := api.GetInstance(ctx, cs.gcp.Project, cs.gcpTarget.Instance)
	switch {
	case httpStatus(err) == http.StatusNotFound:
		return nil, cloudErr(ReasonTargetNotFound, "the Cloud SQL instance does not exist")
	case err != nil:
		return nil, sqlAdminErr(err)
	}
	if !strings.HasPrefix(in.DatabaseVersion, "POSTGRES") {
		return nil, cloudErr(ReasonCloudError, "the Cloud SQL instance is not a PostgreSQL instance")
	}
	if in.ConnectionName != cs.connectionName() {
		return nil, cloudErr(ReasonTargetNotFound, "the Cloud SQL instance is not in the named project and location")
	}
	ip := pickIP(in.IpAddresses, allowPrivate)
	if ip == "" {
		return nil, cloudErr(ReasonCloudError, "the Cloud SQL instance has no usable IP address")
	}
	d := map[string]string{
		dbv1alpha1.DiscoveredEndpoint:               ip,
		dbv1alpha1.DiscoveredPort:                   cloudSQLPort,
		dbv1alpha1.DiscoveredInstanceConnectionName: in.ConnectionName,
	}
	if in.Region != "" {
		d[dbv1alpha1.DiscoveredRegion] = in.Region
	}
	// The DNS name is what a server certificate with a host name carries (the
	// API reports it with a trailing dot); only a plain name is used as the TLS
	// server name.
	if dns := strings.TrimSuffix(in.DnsName, "."); validEndpoint(dns) {
		d[dbv1alpha1.DiscoveredDNSName] = dns
	}
	return d, nil
}

// pickIP chooses the instance's PRIVATE or PRIMARY address. Anything that is
// not a routable unicast address is ignored: the admin password is sent to it.
func pickIP(ips []*sqladmin.IpMapping, allowPrivate bool) string {
	var private, public string
	for _, m := range ips {
		a, err := netip.ParseAddr(m.IpAddress)
		if err == nil {
			a = a.Unmap()
		}
		if err != nil || a.IsUnspecified() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() {
			continue
		}
		switch m.Type {
		case "PRIVATE":
			private = a.String()
		case "PRIMARY":
			public = a.String()
		}
	}
	if allowPrivate && private != "" {
		return private
	}
	return firstNonEmpty(public, private)
}

// waitOperation follows a long-running operation of the Admin API until it is
// done, and returns a category for a failed one. It gives up after
// sqlAdminWait.
func waitOperation(ctx context.Context, api SQLAdminAPI, project string, op *sqladmin.Operation) error {
	ctx, cancel := context.WithTimeout(ctx, sqlAdminWait)
	defer cancel()
	for {
		if op.Status == "DONE" {
			if op.Error != nil && len(op.Error.Errors) > 0 {
				return cloudErr(ReasonCloudError, "the Cloud SQL Admin API operation failed")
			}
			return nil
		}
		t := time.NewTimer(sqlAdminPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return cloudErr(ReasonCloudError, "the Cloud SQL Admin API operation did not finish in time")
		case <-t.C:
		}
		var err error
		if op, err = api.GetOperation(ctx, project, op.Name); err != nil {
			return sqlAdminErr(err)
		}
	}
}

// insertIAMUser creates the Cloud SQL IAM database user named name, and waits
// until it exists.
func insertIAMUser(ctx context.Context, api SQLAdminAPI, cs *cloudSpec, name string) error {
	op, err := api.InsertUser(ctx, cs.gcp.Project, cs.gcpTarget.Instance, &sqladmin.User{Name: name, Type: principalType(name)})
	if err != nil {
		return sqlAdminErr(err)
	}
	return waitOperation(ctx, api, cs.gcp.Project, op)
}

// deleteIAMUser removes the Cloud SQL IAM database user, which drops its role,
// and waits until it is gone. A user that is gone already is not an error.
func deleteIAMUser(ctx context.Context, api SQLAdminAPI, cs *cloudSpec, name string) error {
	op, err := api.DeleteUser(ctx, cs.gcp.Project, cs.gcpTarget.Instance, name)
	switch {
	case httpStatus(err) == http.StatusNotFound:
		return nil
	case err != nil:
		return sqlAdminErr(err)
	}
	return waitOperation(ctx, api, cs.gcp.Project, op)
}
