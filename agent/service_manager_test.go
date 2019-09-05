package agent

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/sdk/testutil"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/hashicorp/consul/testrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceManager_RegisterService(t *testing.T) {
	require := require.New(t)

	a := NewTestAgent(t, t.Name(), "enable_central_service_config = true")
	defer a.Shutdown()

	testrpc.WaitForLeader(t, a.RPC, "dc1")

	// Register a global proxy and service config
	testApplyConfigEntries(t, a,
		&structs.ProxyConfigEntry{
			Config: map[string]interface{}{
				"foo": 1,
			},
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "redis",
			Protocol: "tcp",
		},
	)

	// Now register a service locally with no sidecar, it should be a no-op.
	svc := &structs.NodeService{
		ID:      "redis",
		Service: "redis",
		Port:    8000,
	}
	require.NoError(a.AddService(svc, nil, false, "", ConfigSourceLocal))

	// Verify both the service and sidecar.
	redisService := a.State.Service("redis")
	require.NotNil(redisService)
	require.Equal(&structs.NodeService{
		ID:      "redis",
		Service: "redis",
		Port:    8000,
		Weights: &structs.Weights{
			Passing: 1,
			Warning: 1,
		},
	}, redisService)
}

func TestServiceManager_RegisterSidecar(t *testing.T) {
	require := require.New(t)

	a := NewTestAgent(t, t.Name(), "enable_central_service_config = true")
	defer a.Shutdown()

	testrpc.WaitForLeader(t, a.RPC, "dc1")

	// Register a global proxy and service config
	testApplyConfigEntries(t, a,
		&structs.ProxyConfigEntry{
			Config: map[string]interface{}{
				"foo": 1,
			},
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "web",
			Protocol: "http",
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "redis",
			Protocol: "tcp",
		},
	)

	// Now register a sidecar proxy. Note we don't use SidecarService here because
	// that gets resolved earlier in config handling than the AddService call
	// here.
	svc := &structs.NodeService{
		Kind:    structs.ServiceKindConnectProxy,
		ID:      "web-sidecar-proxy",
		Service: "web-sidecar-proxy",
		Port:    21000,
		Proxy: structs.ConnectProxyConfig{
			DestinationServiceName: "web",
			DestinationServiceID:   "web",
			LocalServiceAddress:    "127.0.0.1",
			LocalServicePort:       8000,
			Upstreams: structs.Upstreams{
				{
					DestinationName: "redis",
					LocalBindPort:   5000,
				},
			},
		},
	}
	require.NoError(a.AddService(svc, nil, false, "", ConfigSourceLocal))

	// Verify sidecar got global config loaded
	sidecarService := a.State.Service("web-sidecar-proxy")
	require.NotNil(sidecarService)
	require.Equal(&structs.NodeService{
		Kind:    structs.ServiceKindConnectProxy,
		ID:      "web-sidecar-proxy",
		Service: "web-sidecar-proxy",
		Port:    21000,
		Proxy: structs.ConnectProxyConfig{
			DestinationServiceName: "web",
			DestinationServiceID:   "web",
			LocalServiceAddress:    "127.0.0.1",
			LocalServicePort:       8000,
			Config: map[string]interface{}{
				"foo":      int64(1),
				"protocol": "http",
			},
			Upstreams: structs.Upstreams{
				{
					DestinationName: "redis",
					LocalBindPort:   5000,
					Config: map[string]interface{}{
						"protocol": "tcp",
					},
				},
			},
		},
		Weights: &structs.Weights{
			Passing: 1,
			Warning: 1,
		},
	}, sidecarService)
}

func TestServiceManager_RegisterMeshGateway(t *testing.T) {
	require := require.New(t)

	a := NewTestAgent(t, t.Name(), "enable_central_service_config = true")
	defer a.Shutdown()

	testrpc.WaitForLeader(t, a.RPC, "dc1")

	// Register a global proxy and service config
	testApplyConfigEntries(t, a,
		&structs.ProxyConfigEntry{
			Config: map[string]interface{}{
				"foo": 1,
			},
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "mesh-gateway",
			Protocol: "http",
		},
	)

	// Now register a mesh-gateway.
	svc := &structs.NodeService{
		Kind:    structs.ServiceKindMeshGateway,
		ID:      "mesh-gateway",
		Service: "mesh-gateway",
		Port:    443,
	}

	require.NoError(a.AddService(svc, nil, false, "", ConfigSourceLocal))

	// Verify gateway got global config loaded
	gateway := a.State.Service("mesh-gateway")
	require.NotNil(gateway)
	require.Equal(&structs.NodeService{
		Kind:    structs.ServiceKindMeshGateway,
		ID:      "mesh-gateway",
		Service: "mesh-gateway",
		Port:    443,
		Proxy: structs.ConnectProxyConfig{
			Config: map[string]interface{}{
				"foo":      int64(1),
				"protocol": "http",
			},
		},
		Weights: &structs.Weights{
			Passing: 1,
			Warning: 1,
		},
	}, gateway)
}

func TestServiceManager_PersistService_API(t *testing.T) {
	// This is the ServiceManager version of TestAgent_PersistService  and
	// TestAgent_PurgeService.
	t.Parallel()

	require := require.New(t)

	// Launch a server to manage the config entries.
	serverAgent := NewTestAgent(t, t.Name(), `enable_central_service_config = true`)
	defer serverAgent.Shutdown()
	testrpc.WaitForLeader(t, serverAgent.RPC, "dc1")

	// Register a global proxy and service config
	testApplyConfigEntries(t, serverAgent,
		&structs.ProxyConfigEntry{
			Config: map[string]interface{}{
				"foo": 1,
			},
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "web",
			Protocol: "http",
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "redis",
			Protocol: "tcp",
		},
	)

	// Now launch a single client agent
	dataDir := testutil.TempDir(t, "agent") // we manage the data dir
	defer os.RemoveAll(dataDir)

	cfg := `
	    enable_central_service_config = true
		server = false
		bootstrap = false
		data_dir = "` + dataDir + `"
	`
	a := NewTestAgentWithFields(t, true, TestAgent{HCL: cfg, DataDir: dataDir})
	defer a.Shutdown()

	// Join first
	_, err := a.JoinLAN([]string{
		fmt.Sprintf("127.0.0.1:%d", serverAgent.Config.SerfPortLAN),
	})
	require.NoError(err)

	testrpc.WaitForLeader(t, a.RPC, "dc1")

	// Now register a sidecar proxy via the API.
	svc := &structs.NodeService{
		Kind:    structs.ServiceKindConnectProxy,
		ID:      "web-sidecar-proxy",
		Service: "web-sidecar-proxy",
		Port:    21000,
		Proxy: structs.ConnectProxyConfig{
			DestinationServiceName: "web",
			DestinationServiceID:   "web",
			LocalServiceAddress:    "127.0.0.1",
			LocalServicePort:       8000,
			Upstreams: structs.Upstreams{
				{
					DestinationName: "redis",
					LocalBindPort:   5000,
				},
			},
		},
	}

	expectState := &structs.NodeService{
		Kind:    structs.ServiceKindConnectProxy,
		ID:      "web-sidecar-proxy",
		Service: "web-sidecar-proxy",
		Port:    21000,
		Proxy: structs.ConnectProxyConfig{
			DestinationServiceName: "web",
			DestinationServiceID:   "web",
			LocalServiceAddress:    "127.0.0.1",
			LocalServicePort:       8000,
			Config: map[string]interface{}{
				"foo":      int64(1),
				"protocol": "http",
			},
			Upstreams: structs.Upstreams{
				{
					DestinationName: "redis",
					LocalBindPort:   5000,
					Config: map[string]interface{}{
						"protocol": "tcp",
					},
				},
			},
		},
		Weights: &structs.Weights{
			Passing: 1,
			Warning: 1,
		},
	}

	svcFile := filepath.Join(a.Config.DataDir, servicesDir, stringHash(svc.ID))
	configFile := filepath.Join(a.Config.DataDir, serviceConfigDir, stringHash(svc.ID))

	// Service is not persisted unless requested, but we always persist service configs.
	require.NoError(a.AddService(svc, nil, false, "", ConfigSourceRemote))
	requireFileIsAbsent(t, svcFile)
	requireFileIsPresent(t, configFile)

	// Persists to file if requested
	require.NoError(a.AddService(svc, nil, true, "mytoken", ConfigSourceRemote))
	requireFileIsPresent(t, svcFile)
	requireFileIsPresent(t, configFile)

	// Service definition file is sane.
	expectJSONFile(t, svcFile, persistedService{
		Token:   "mytoken",
		Service: svc,
		Source:  "remote",
	})

	// Service config file is sane.
	expectJSONFile(t, configFile, persistedServiceConfig{
		ServiceID: "web-sidecar-proxy",
		Defaults: &persistedServiceConfigResponse{
			ProxyConfig: map[string]interface{}{
				"foo":      1,
				"protocol": "http",
			},
			UpstreamConfigs: map[string]map[string]interface{}{
				"redis": map[string]interface{}{
					"protocol": "tcp",
				},
			},
		},
	})

	// Verify in memory state.
	{
		sidecarService := a.State.Service("web-sidecar-proxy")
		require.NotNil(sidecarService)
		require.Equal(expectState, sidecarService)
	}

	// Updates service definition on disk
	svc.Proxy.LocalServicePort = 8001
	require.NoError(a.AddService(svc, nil, true, "mytoken", ConfigSourceRemote))
	requireFileIsPresent(t, svcFile)
	requireFileIsPresent(t, configFile)

	// Service definition file is updated.
	expectJSONFile(t, svcFile, persistedService{
		Token:   "mytoken",
		Service: svc,
		Source:  "remote",
	})

	// Service config file is the same.
	expectJSONFile(t, configFile, persistedServiceConfig{
		ServiceID: "web-sidecar-proxy",
		Defaults: &persistedServiceConfigResponse{
			ProxyConfig: map[string]interface{}{
				"foo":      1,
				"protocol": "http",
			},
			UpstreamConfigs: map[string]map[string]interface{}{
				"redis": map[string]interface{}{
					"protocol": "tcp",
				},
			},
		},
	})

	// Verify in memory state.
	expectState.Proxy.LocalServicePort = 8001
	{
		sidecarService := a.State.Service("web-sidecar-proxy")
		require.NotNil(sidecarService)
		require.Equal(expectState, sidecarService)
	}

	// Kill the agent to restart it.
	a.Shutdown()

	// Kill the server so that it can't phone home and must rely upon the persisted defaults.
	serverAgent.Shutdown()

	// Should load it back during later start.
	a2 := NewTestAgentWithFields(t, true, TestAgent{HCL: cfg, DataDir: dataDir})
	defer a2.Shutdown()

	{
		restored := a.State.Service("web-sidecar-proxy")
		require.NotNil(restored)
		require.Equal(expectState, restored)
	}

	// Now remove it.
	require.NoError(a2.RemoveService("web-sidecar-proxy"))
	requireFileIsAbsent(t, svcFile)
	requireFileIsAbsent(t, configFile)
}

func TestServiceManager_PersistService_ConfigFiles(t *testing.T) {
	// This is the ServiceManager version of TestAgent_PersistService  and
	// TestAgent_PurgeService but for config files.
	t.Parallel()

	require := require.New(t)

	// Launch a server to manage the config entries.
	serverAgent := NewTestAgent(t, t.Name(), `enable_central_service_config = true`)
	defer serverAgent.Shutdown()
	testrpc.WaitForLeader(t, serverAgent.RPC, "dc1")

	// Register a global proxy and service config
	testApplyConfigEntries(t, serverAgent,
		&structs.ProxyConfigEntry{
			Config: map[string]interface{}{
				"foo": 1,
			},
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "web",
			Protocol: "http",
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "redis",
			Protocol: "tcp",
		},
	)

	// Now launch a single client agent
	dataDir := testutil.TempDir(t, "agent") // we manage the data dir
	defer os.RemoveAll(dataDir)

	serviceSnippet := `
		service = {
		  kind  = "connect-proxy"
		  id    = "web-sidecar-proxy"
		  name  = "web-sidecar-proxy"
		  port  = 21000
		  token = "mytoken"
		  proxy {
			destination_service_name = "web"
			destination_service_id   = "web"
			local_service_address    = "127.0.0.1"
			local_service_port       = 8000
			upstreams = [{
			  destination_name = "redis"
			  local_bind_port  = 5000
			}]
		  }
		}
	`

	cfg := `
	    enable_central_service_config = true
		data_dir = "` + dataDir + `"
		server = false
		bootstrap = false
	` + serviceSnippet

	a := NewTestAgentWithFields(t, true, TestAgent{HCL: cfg, DataDir: dataDir})
	defer a.Shutdown()

	// Join first
	_, err := a.JoinLAN([]string{
		fmt.Sprintf("127.0.0.1:%d", serverAgent.Config.SerfPortLAN),
	})
	require.NoError(err)

	testrpc.WaitForLeader(t, a.RPC, "dc1")

	// Now register a sidecar proxy via the API.
	svcID := "web-sidecar-proxy"

	expectState := &structs.NodeService{
		Kind:    structs.ServiceKindConnectProxy,
		ID:      "web-sidecar-proxy",
		Service: "web-sidecar-proxy",
		Port:    21000,
		Proxy: structs.ConnectProxyConfig{
			DestinationServiceName: "web",
			DestinationServiceID:   "web",
			LocalServiceAddress:    "127.0.0.1",
			LocalServicePort:       8000,
			Config: map[string]interface{}{
				"foo":      int64(1),
				"protocol": "http",
			},
			Upstreams: structs.Upstreams{
				{
					DestinationType: "service",
					DestinationName: "redis",
					LocalBindPort:   5000,
					Config: map[string]interface{}{
						"protocol": "tcp",
					},
				},
			},
		},
		Weights: &structs.Weights{
			Passing: 1,
			Warning: 1,
		},
	}

	// Now wait until we've re-registered using central config updated data.
	retry.Run(t, func(r *retry.R) {
		a.stateLock.Lock()
		defer a.stateLock.Unlock()
		current := a.State.Service("web-sidecar-proxy")
		if current == nil {
			r.Fatalf("service is missing")
		}
		if !assert.ObjectsAreEqual(expectState, current) {
			r.Fatalf("expected: %#v\nactual  :%#v", expectState, current)
		}
	})

	svcFile := filepath.Join(a.Config.DataDir, servicesDir, stringHash(svcID))
	configFile := filepath.Join(a.Config.DataDir, serviceConfigDir, stringHash(svcID))

	// Service is never persisted, but we always persist service configs.
	requireFileIsAbsent(t, svcFile)
	requireFileIsPresent(t, configFile)

	// Service config file is sane.
	expectJSONFile(t, configFile, persistedServiceConfig{
		ServiceID: "web-sidecar-proxy",
		Defaults: &persistedServiceConfigResponse{
			ProxyConfig: map[string]interface{}{
				"foo":      1,
				"protocol": "http",
			},
			UpstreamConfigs: map[string]map[string]interface{}{
				"redis": map[string]interface{}{
					"protocol": "tcp",
				},
			},
		},
	})

	// Verify in memory state.
	{
		sidecarService := a.State.Service("web-sidecar-proxy")
		require.NotNil(sidecarService)
		require.Equal(expectState, sidecarService)
	}

	// Kill the agent to restart it.
	a.Shutdown()

	// Kill the server so that it can't phone home and must rely upon the persisted defaults.
	serverAgent.Shutdown()

	// Should load it back during later start.
	a2 := NewTestAgentWithFields(t, true, TestAgent{HCL: cfg, DataDir: dataDir})
	defer a2.Shutdown()

	{
		restored := a.State.Service("web-sidecar-proxy")
		require.NotNil(restored)
		require.Equal(expectState, restored)
	}

	// Now remove it.
	require.NoError(a2.RemoveService("web-sidecar-proxy"))
	requireFileIsAbsent(t, svcFile)
	requireFileIsAbsent(t, configFile)
}

func TestServiceManager_Disabled(t *testing.T) {
	require := require.New(t)

	a := NewTestAgent(t, t.Name(), "enable_central_service_config = false")
	defer a.Shutdown()

	testrpc.WaitForLeader(t, a.RPC, "dc1")

	// Register a global proxy and service config
	testApplyConfigEntries(t, a,
		&structs.ProxyConfigEntry{
			Config: map[string]interface{}{
				"foo": 1,
			},
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "web",
			Protocol: "http",
		},
		&structs.ServiceConfigEntry{
			Kind:     structs.ServiceDefaults,
			Name:     "redis",
			Protocol: "tcp",
		},
	)

	// Now register a sidecar proxy. Note we don't use SidecarService here because
	// that gets resolved earlier in config handling than the AddService call
	// here.
	svc := &structs.NodeService{
		Kind:    structs.ServiceKindConnectProxy,
		ID:      "web-sidecar-proxy",
		Service: "web-sidecar-proxy",
		Port:    21000,
		Proxy: structs.ConnectProxyConfig{
			DestinationServiceName: "web",
			DestinationServiceID:   "web",
			LocalServiceAddress:    "127.0.0.1",
			LocalServicePort:       8000,
			Upstreams: structs.Upstreams{
				{
					DestinationName: "redis",
					LocalBindPort:   5000,
				},
			},
		},
	}
	require.NoError(a.AddService(svc, nil, false, "", ConfigSourceLocal))

	// Verify sidecar got global config loaded
	sidecarService := a.State.Service("web-sidecar-proxy")
	require.NotNil(sidecarService)
	require.Equal(&structs.NodeService{
		Kind:    structs.ServiceKindConnectProxy,
		ID:      "web-sidecar-proxy",
		Service: "web-sidecar-proxy",
		Port:    21000,
		Proxy: structs.ConnectProxyConfig{
			DestinationServiceName: "web",
			DestinationServiceID:   "web",
			LocalServiceAddress:    "127.0.0.1",
			LocalServicePort:       8000,
			// No config added
			Upstreams: structs.Upstreams{
				{
					DestinationName: "redis",
					LocalBindPort:   5000,
					// No config added
				},
			},
		},
		Weights: &structs.Weights{
			Passing: 1,
			Warning: 1,
		},
	}, sidecarService)
}

func testApplyConfigEntries(t *testing.T, a *TestAgent, entries ...structs.ConfigEntry) {
	t.Helper()
	for _, entry := range entries {
		args := &structs.ConfigEntryRequest{
			Datacenter: "dc1",
			Entry:      entry,
		}
		var out bool
		require.NoError(t, a.RPC("ConfigEntry.Apply", args, &out))
	}
}

func requireFileIsAbsent(t *testing.T, file string) {
	t.Helper()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("should not persist")
	}
}

func requireFileIsPresent(t *testing.T, file string) {
	t.Helper()
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func expectJSONFile(t *testing.T, file string, expect interface{}) {
	t.Helper()

	expected, err := json.Marshal(expect)
	require.NoError(t, err)

	content, err := ioutil.ReadFile(file)
	require.NoError(t, err)

	require.JSONEq(t, string(expected), string(content))
}
