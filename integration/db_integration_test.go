/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gravitational/teleport/lib"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/service"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/db/postgres"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/jackc/pgconn"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/require"
)

// TestDatabaseAccessRootCluster tests a scenario where a user connects to a
// database service running in a root cluster.
func TestDatabaseAccessRootCluster(t *testing.T) {
	pack := setupDatabaseTest(t)

	// Connect to the database service in root cluster.
	client, err := postgres.MakeTestClient(context.Background(), postgres.TestClientConfig{
		AuthClient: pack.rootCluster.GetSiteAPI(pack.rootCluster.Secrets.SiteName),
		AuthServer: pack.rootCluster.Process.GetAuthServer(),
		Address:    fmt.Sprintf("%v:%v", Loopback, pack.rootCluster.GetPortWeb()),
		Cluster:    pack.rootCluster.Secrets.SiteName,
		Username:   pack.rootUser.GetName(),
		RouteToDatabase: tlsca.RouteToDatabase{
			ServiceName: pack.rootDBService.Name,
			Protocol:    pack.rootDBService.Protocol,
			Username:    "postgres",
			Database:    "test",
		},
	})
	require.NoError(t, err)

	// Execute a query.
	result, err := client.Exec(context.Background(), "select 1").ReadAll()
	require.NoError(t, err)
	require.Equal(t, []*pgconn.Result{postgres.TestQueryResponse}, result)
	require.Equal(t, uint32(1), pack.rootPostgres.QueryCount())
	require.Equal(t, uint32(0), pack.leafPostgres.QueryCount())

	// Disconnect.
	err = client.Close(context.Background())
	require.NoError(t, err)
}

// TestDatabaseAccessLeafCluster tests a scenario where a user connects to a
// database service running in a leaf cluster via a root cluster.
func TestDatabaseAccessLeafCluster(t *testing.T) {
	pack := setupDatabaseTest(t)

	// Connect to the database service in leaf cluster via root cluster.
	client, err := postgres.MakeTestClient(context.Background(), postgres.TestClientConfig{
		AuthClient: pack.rootCluster.GetSiteAPI(pack.rootCluster.Secrets.SiteName),
		AuthServer: pack.rootCluster.Process.GetAuthServer(),
		Address:    fmt.Sprintf("%v:%v", Loopback, pack.rootCluster.GetPortWeb()), // Connecting via root cluster.
		Cluster:    pack.leafCluster.Secrets.SiteName,
		Username:   pack.rootUser.GetName(),
		RouteToDatabase: tlsca.RouteToDatabase{
			ServiceName: pack.leafDBService.Name,
			Protocol:    pack.leafDBService.Protocol,
			Username:    "postgres",
			Database:    "test",
		},
	})
	require.NoError(t, err)

	// Execute a query.
	result, err := client.Exec(context.Background(), "select 1").ReadAll()
	require.NoError(t, err)
	require.Equal(t, []*pgconn.Result{postgres.TestQueryResponse}, result)
	require.Equal(t, uint32(1), pack.leafPostgres.QueryCount())
	require.Equal(t, uint32(0), pack.rootPostgres.QueryCount())

	// Disconnect.
	err = client.Close(context.Background())
	require.NoError(t, err)
}

type databasePack struct {
	rootCluster *TeleInstance
	leafCluster *TeleInstance

	rootUser services.User
	leafUser services.User

	rootRole services.Role
	leafRole services.Role

	rootDBService service.Database
	leafDBService service.Database

	rootDBProcess *service.TeleportProcess
	leafDBProcess *service.TeleportProcess

	rootDBAuthClient *auth.Client
	leafDBAuthClient *auth.Client

	rootPostgresAddr string
	leafPostgresAddr string

	rootPostgres *postgres.TestServer
	leafPostgres *postgres.TestServer
}

func setupDatabaseTest(t *testing.T) *databasePack {
	// Some global setup.
	tracer := utils.NewTracer(utils.ThisFunction()).Start()
	t.Cleanup(func() { tracer.Stop() })
	utils.InitLoggerForTests(testing.Verbose())
	lib.SetInsecureDevMode(true)
	SetTestTimeouts(100 * time.Millisecond)

	// Create ports allocator.
	startPort := utils.PortStartingNumber + (3 * AllocatePortsNum) + 1
	ports, err := utils.GetFreeTCPPorts(AllocatePortsNum, startPort)
	require.NoError(t, err)

	// Generate keypair.
	privateKey, publicKey, err := testauthority.New().GenerateKeyPair("")
	require.NoError(t, err)

	p := &databasePack{
		rootPostgresAddr: fmt.Sprintf("localhost:%v", ports.PopInt()),
		leafPostgresAddr: fmt.Sprintf("localhost:%v", ports.PopInt()),
	}

	// Create root cluster.
	p.rootCluster = NewInstance(InstanceConfig{
		ClusterName: "root.example.com",
		HostID:      uuid.New(),
		NodeName:    Host,
		Ports:       ports.PopIntSlice(5),
		Priv:        privateKey,
		Pub:         publicKey,
	})

	// Create leaf cluster.
	p.leafCluster = NewInstance(InstanceConfig{
		ClusterName: "leaf.example.com",
		HostID:      uuid.New(),
		NodeName:    Host,
		Ports:       ports.PopIntSlice(5),
		Priv:        privateKey,
		Pub:         publicKey,
	})

	// Make root cluster config.
	rcConf := service.MakeDefaultConfig()
	rcConf.DataDir = t.TempDir()
	rcConf.Auth.Enabled = true
	rcConf.Auth.Preference.SetSecondFactor("off")
	rcConf.Proxy.Enabled = true
	rcConf.Proxy.DisableWebInterface = true

	// Make leaf cluster config.
	lcConf := service.MakeDefaultConfig()
	lcConf.DataDir = t.TempDir()
	lcConf.Auth.Enabled = true
	lcConf.Auth.Preference.SetSecondFactor("off")
	lcConf.Proxy.Enabled = true
	lcConf.Proxy.DisableWebInterface = true

	// Establish trust b/w root and leaf.
	err = p.rootCluster.CreateEx(p.leafCluster.Secrets.AsSlice(), rcConf)
	require.NoError(t, err)
	err = p.leafCluster.CreateEx(p.rootCluster.Secrets.AsSlice(), lcConf)
	require.NoError(t, err)

	// Start both clusters.
	err = p.leafCluster.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		p.leafCluster.StopAll()
	})
	err = p.rootCluster.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		p.rootCluster.StopAll()
	})

	// Setup users and roles on both clusters.
	p.setupUsersAndRoles(t)

	// Update root's certificate authority on leaf to configure role mapping.
	ca, err := p.leafCluster.Process.GetAuthServer().GetCertAuthority(services.CertAuthID{
		Type:       services.UserCA,
		DomainName: p.rootCluster.Secrets.SiteName,
	}, false)
	require.NoError(t, err)
	ca.SetRoles(nil) // Reset roles, otherwise they will take precedence.
	ca.SetRoleMap(services.RoleMap{
		{Remote: p.rootRole.GetName(), Local: []string{p.leafRole.GetName()}},
	})
	err = p.leafCluster.Process.GetAuthServer().UpsertCertAuthority(ca)
	require.NoError(t, err)

	// Create and start database service in the root cluster.
	p.rootDBService = service.Database{
		Name:     "root-postgres",
		Protocol: defaults.ProtocolPostgres,
		URI:      p.rootPostgresAddr,
	}
	rdConf := service.MakeDefaultConfig()
	rdConf.DataDir = t.TempDir()
	rdConf.Token = "static-token-value"
	rdConf.AuthServers = []utils.NetAddr{
		{
			AddrNetwork: "tcp",
			Addr:        net.JoinHostPort(Loopback, p.rootCluster.GetPortWeb()),
		},
	}
	rdConf.Databases.Enabled = true
	rdConf.Databases.Databases = []service.Database{p.rootDBService}
	p.rootDBProcess, p.rootDBAuthClient, err = p.rootCluster.StartDatabase(rdConf)
	require.NoError(t, err)
	t.Cleanup(func() {
		p.rootDBProcess.Close()
	})

	// Create and start database service in the leaf cluster.
	p.leafDBService = service.Database{
		Name:     "leaf-postgres",
		Protocol: defaults.ProtocolPostgres,
		URI:      p.leafPostgresAddr,
	}
	ldConf := service.MakeDefaultConfig()
	ldConf.DataDir = t.TempDir()
	ldConf.Token = "static-token-value"
	ldConf.AuthServers = []utils.NetAddr{
		{
			AddrNetwork: "tcp",
			Addr:        net.JoinHostPort(Loopback, p.leafCluster.GetPortWeb()),
		},
	}
	ldConf.Databases.Enabled = true
	ldConf.Databases.Databases = []service.Database{p.leafDBService}
	p.leafDBProcess, p.leafDBAuthClient, err = p.leafCluster.StartDatabase(ldConf)
	require.NoError(t, err)
	t.Cleanup(func() {
		p.leafDBProcess.Close()
	})

	// Create and start test Postgres in the root cluster.
	p.rootPostgres, err = postgres.MakeTestServer(p.rootDBAuthClient, p.rootDBService.Name, p.rootPostgresAddr)
	require.NoError(t, err)
	go p.rootPostgres.Serve()
	t.Cleanup(func() {
		p.rootPostgres.Close()
	})

	// Create and start test Postgres in the leaf cluster.
	p.leafPostgres, err = postgres.MakeTestServer(p.leafDBAuthClient, p.leafDBService.Name, p.leafPostgresAddr)
	require.NoError(t, err)
	go p.leafPostgres.Serve()
	t.Cleanup(func() {
		p.leafPostgres.Close()
	})

	return p
}

func (p *databasePack) setupUsersAndRoles(t *testing.T) {
	var err error

	p.rootUser, p.rootRole, err = auth.CreateUserAndRole(p.rootCluster.Process.GetAuthServer(), "root-user", nil)
	require.NoError(t, err)

	p.rootRole.SetDatabaseUsers(services.Allow, []string{services.Wildcard})
	p.rootRole.SetDatabaseNames(services.Allow, []string{services.Wildcard})
	err = p.rootCluster.Process.GetAuthServer().UpsertRole(context.Background(), p.rootRole)
	require.NoError(t, err)

	p.leafUser, p.leafRole, err = auth.CreateUserAndRole(p.rootCluster.Process.GetAuthServer(), "leaf-user", nil)
	require.NoError(t, err)

	p.leafRole.SetDatabaseUsers(services.Allow, []string{services.Wildcard})
	p.leafRole.SetDatabaseNames(services.Allow, []string{services.Wildcard})
	err = p.leafCluster.Process.GetAuthServer().UpsertRole(context.Background(), p.leafRole)
	require.NoError(t, err)
}
