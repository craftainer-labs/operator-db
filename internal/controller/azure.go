package controller

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"

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
	var ne net.Error
	var af *azidentity.AuthenticationFailedError
	st := armStatus(err)
	isAuth := errors.As(err, &af)
	if isAuth && af.RawResponse != nil {
		st = af.RawResponse.StatusCode
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.As(err, &ne) ||
		st == http.StatusTooManyRequests || st == http.StatusRequestTimeout || st >= 500:
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

// describeAzure reads the flexible server's host name, region and resource id.
// The host name is what the server's certificate carries, and the admin
// password is sent to it, so only a plain DNS name is accepted. A server that is
// not Ready (stopped, starting, updating) has no usable address for now: it is a transient CloudError.
func describeAzure(ctx context.Context, api ARMAPI, cs *cloudSpec) (map[string]string, error) {
	srv, err := api.GetServer(ctx, cs.azure.ResourceGroup, cs.azureTarget.ServerName)
	switch {
	case armStatus(err) == http.StatusNotFound:
		return nil, cloudErr(ReasonTargetNotFound, "the flexible server does not exist")
	case err != nil:
		return nil, armErr(err)
	}
	p := srv.Properties
	if p == nil || p.FullyQualifiedDomainName == nil || *p.FullyQualifiedDomainName == "" {
		return nil, cloudErr(ReasonCloudError, "the flexible server has no host name yet")
	}
	if p.State != nil && *p.State != armpostgresqlflexibleservers.ServerStateReady {
		// Starting, updating or stopped: the address comes back by itself, so the
		// last one is kept through it.
		return nil, transientErr(ReasonCloudError, "the flexible server is not ready")
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
