package controller

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcloud "github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v6"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	dbv1alpha1 "github.com/craftainer-labs/operator-db/api/v1alpha1"
)

// azurePort is the port every Azure Database for PostgreSQL flexible server
// serves.
const azurePort = "5432"

// ARMAPI is the part of the Azure Resource Manager API the operator uses: the
// PostgreSQL flexible servers client.
type ARMAPI interface {
	GetServer(ctx context.Context, resourceGroup, name string) (armpostgresqlflexibleservers.Server, error)
}

type armServers struct {
	c *armpostgresqlflexibleservers.ServersClient
}

// NewARM builds the flexible servers client of an Azure client for a
// subscription: its HTTP transport dials through the egress policy and its
// credential is the operator's (cloud.Egress.Azure); its endpoint is the
// endpointOverride, if any. Resource provider registration is off: the
// operator only reads.
func NewARM(c *cloud.AzureClient, subscriptionID string) (ARMAPI, error) {
	opts := &arm.ClientOptions{ClientOptions: c.ClientOptions, DisableRPRegistration: true}
	if c.Endpoint != "" {
		// The services map is shared with the credential: change a copy.
		opts.Cloud.Services = maps.Clone(opts.Cloud.Services)
		svc := opts.Cloud.Services[azcloud.ResourceManager]
		svc.Endpoint = c.Endpoint
		opts.Cloud.Services[azcloud.ResourceManager] = svc
	}
	cl, err := armpostgresqlflexibleservers.NewServersClient(subscriptionID, c.Credential, opts)
	if err != nil {
		return nil, err
	}
	return armServers{cl}, nil
}

func (a armServers) GetServer(ctx context.Context, resourceGroup, name string) (armpostgresqlflexibleservers.Server, error) {
	r, err := a.c.Get(ctx, resourceGroup, name, nil)
	return r.Server, err
}

// armStatus is the HTTP status of a failed ARM call, 0 for another error.
func armStatus(err error) int {
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		return re.StatusCode
	}
	return 0
}

// armErr classifies a failed ARM call or Entra token request: throttling,
// timeouts and 5xx answers are transient (the last address is kept), a refused
// credential is CredentialsUnavailable, everything else (access denied, a bad
// request) is not transient. The SDK's text is never kept: it names the
// subscription, the resource group and the server.
func armErr(err error) error {
	var af *azidentity.AuthenticationFailedError
	st := armStatus(err)
	isAuth := errors.As(err, &af)
	if isAuth && af.RawResponse != nil {
		st = af.RawResponse.StatusCode
	}
	switch {
	case transientCause(err, st):
		return transientErr(ReasonCloudError, "the Azure Resource Manager call failed")
	case isAuth && af.RawResponse == nil:
		// The authority gave no answer (a blocked or failed connection, a TLS
		// failure): that says nothing about the credential, so it is retried.
		return transientErr(ReasonCloudError, "the Entra authority could not be reached")
	case isAuth:
		return cloudErr(ReasonCredentialsUnavailable, "the Azure credentials were refused")
	}
	return cloudErr(ReasonCloudError, "the Azure Resource Manager call failed")
}

// azureAPI builds the ARM client of an Azure server with the operator's
// credentials to the cloud, through the egress policy and the server's proxy.
// done releases the client's idle connections.
func (c Config) azureAPI(ctx context.Context, cs *cloudSpec, conn server.Connection) (api ARMAPI, done func(), err error) {
	secret, err := c.cloudSecret(ctx, cs)
	if err != nil {
		return nil, nil, err
	}
	e := cloud.Egress{Mode: c.EgressMode, Policy: c.Egress, Proxy: conn.Proxy, RootCAs: c.CloudRootCAs}
	ac, err := e.Azure(ctx, cs.scope, cs.azure, cs.creds, secret)
	if err != nil {
		return nil, nil, egressErr(err)
	}
	if c.ARM == nil {
		return nil, nil, cloudErr(ReasonCloudError, "no Azure Resource Manager client is configured")
	}
	api, err = c.ARM(ac, cs.azure.SubscriptionID)
	if err != nil {
		return nil, nil, cloudErr(ReasonCloudError, "the Azure Resource Manager client could not be built")
	}
	done = func() {}
	if hc, ok := ac.ClientOptions.Transport.(interface{ CloseIdleConnections() }); ok {
		done = hc.CloseIdleConnections
	}
	return api, done, nil
}

// discoverAzure resolves a flexible server to its address with servers.get.
func (c Config) discoverAzure(ctx context.Context, cs *cloudSpec, conn server.Connection) (map[string]string, error) {
	api, done, err := c.azureAPI(ctx, cs, conn)
	if err != nil {
		return nil, err
	}
	defer done()
	return describeAzure(ctx, api, cs)
}

// recovering says the state passes by itself.
func recovering(st armpostgresqlflexibleservers.ServerState) bool {
	return st == armpostgresqlflexibleservers.ServerStateStarting || st == armpostgresqlflexibleservers.ServerStateUpdating
}

// describeAzure reads the flexible server's host name, region and resource id.
// The host name is what the server's certificate carries, and the admin
// password is sent to it, so only a plain DNS name is accepted. A server that is
// not Ready has no usable address: a CloudError, transient only while it is
// Starting or Updating (it comes back by itself). Any other state (Stopping,
// Stopped, Disabled, Dropping, or one Azure adds) is down until somebody acts,
// and the last address is dropped.
func describeAzure(ctx context.Context, api ARMAPI, cs *cloudSpec) (map[string]string, error) {
	srv, err := api.GetServer(ctx, cs.azure.ResourceGroup, cs.azureTarget.ServerName)
	switch {
	case armStatus(err) == http.StatusNotFound:
		return nil, cloudErr(ReasonTargetNotFound, "the flexible server does not exist")
	case err != nil:
		return nil, armErr(err)
	}
	p := srv.Properties
	if p == nil {
		return nil, cloudErr(ReasonCloudError, "the flexible server has no host name yet")
	}
	// The state comes first: a server that is starting may have no host name
	// for a moment.
	if p.State != nil && *p.State != armpostgresqlflexibleservers.ServerStateReady {
		if recovering(*p.State) {
			return nil, transientErr(ReasonCloudError, "the flexible server is not ready")
		}
		return nil, cloudErr(ReasonCloudError, "the flexible server is not ready")
	}
	if p.FullyQualifiedDomainName == nil || *p.FullyQualifiedDomainName == "" {
		return nil, cloudErr(ReasonCloudError, "the flexible server has no host name yet")
	}
	host := *p.FullyQualifiedDomainName
	if !validEndpoint(host) {
		return nil, cloudErr(ReasonCloudError, "the flexible server host name is not a DNS name")
	}
	d := map[string]string{dbv1alpha1.DiscoveredEndpoint: host, dbv1alpha1.DiscoveredPort: azurePort}
	if srv.Location != nil && *srv.Location != "" {
		d[dbv1alpha1.DiscoveredRegion] = *srv.Location
	}
	if srv.ID != nil && *srv.ID != "" {
		d[dbv1alpha1.DiscoveredResourceID] = *srv.ID
	}
	return d, nil
}

// entraLogin is the Entra administrator login of a server with adminAuthMode
// IAM: the principal's name is the PostgreSQL user, and token is the password
// of every new connection.
type entraLogin struct {
	user  string
	token func(context.Context) (string, error)
}

// entraLogin returns the login of cs when its adminAuthMode is IAM, else nil.
// The token source is built on the first token, so a pool that is reused builds
// none (see entraSource).
func (c Config) entraLogin(cs *cloudSpec, cc server.CloudConnection) *entraLogin {
	if cs == nil || !cs.adminIAM {
		return nil
	}
	return &entraLogin{user: cs.entraAdmin, token: c.entraSource(cs, cc)}
}

// entraSource returns the function that fetches an Entra token for the scope of
// Azure Database for PostgreSQL (cloud.AzurePostgresScope) with the operator's
// cloud credentials: the same identity that reads Resource Manager, through the
// egress policy and the server's proxy (cloud.Egress.Azure). The client is
// built on the first call and kept by the function (so a connection pool keeps
// it), and a token is fetched just before a connect and reused until a minute
// before it expires (cloud.CachedSource). Idle connections to the authority are
// closed after each fetch, because a pool outlives the reconcile that built
// it. Errors are categories (cloud.ErrCredentialsUnavailable): the SDK's text
// is not kept.
func (c Config) entraSource(cs *cloudSpec, cc server.CloudConnection) func(context.Context) (string, error) {
	var (
		mu      sync.Mutex
		src     *cloud.CachedSource
		release func()
	)
	// build returns the token source, building it on first use. Only a build
	// that worked is kept: a failed one (a refused override, a Secret not yet
	// readable, a cancelled context) is tried again by the next connect.
	build := func(ctx context.Context) (*cloud.CachedSource, func(), error) {
		mu.Lock()
		defer mu.Unlock()
		if src != nil {
			return src, release, nil
		}
		e := cloud.Egress{Mode: c.EgressMode, Policy: c.Egress, Proxy: cc.Proxy, RootCAs: c.CloudRootCAs}
		ac, err := e.Azure(ctx, cs.scope, cs.azure, cs.creds, cc.Credentials)
		if err != nil {
			return nil, nil, egressErr(err)
		}
		rel := func() {}
		if hc, ok := ac.ClientOptions.Transport.(interface{ CloseIdleConnections() }); ok {
			rel = hc.CloseIdleConnections
		}
		src, release = cloud.NewCachedSource(cloud.AzureTokenSource(ac.Credential, cloud.AzurePostgresScope), 0), rel
		return src, release, nil
	}
	return func(ctx context.Context) (string, error) {
		s, rel, err := build(ctx)
		if err != nil {
			return "", err
		}
		tok, _, err := s.Token(ctx)
		rel()
		if err != nil {
			// cloud.ErrCredentialsUnavailable: its text is a category.
			return "", err
		}
		return tok, nil
	}
}

// ErrIAMPrincipalInvalidEntra means iam.principal is not usable as the name of
// an Entra principal's PostgreSQL role.
var ErrIAMPrincipalInvalidEntra = errors.New("IAMPrincipalRequired: iam.principal must be the name of an Entra principal " +
	"(a user's principal name, or a service principal's or managed identity's name), at most 63 bytes, without control characters or " +
	"surrounding spaces")

// ValidEntraPrincipal says whether p can name the PostgreSQL role of an Entra
// principal: pgaadauth_create_principal takes the principal's name, which is
// also the role's name, so it is at most 63 bytes (NAMEDATALEN-1). Whether the
// principal exists in the tenant is for Entra to say.
func ValidEntraPrincipal(p string) bool {
	return p != "" && len(p) <= 63 && utf8.ValidString(p) && p == strings.TrimSpace(p) &&
		!strings.ContainsFunc(p, unicode.IsControl)
}
