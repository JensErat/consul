package agent

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hashicorp/consul/agent/consul/autopilot"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/raft"
)

// OperatorRaftConfiguration is used to inspect the current Raft configuration.
// This supports the stale query mode in case the cluster doesn't have a leader.
func (s *HTTPServer) OperatorRaftConfiguration(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	var args structs.DCSpecificRequest
	if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
		return nil, nil
	}

	var reply structs.RaftConfigurationResponse
	if err := s.agent.RPC("Operator.RaftGetConfiguration", &args, &reply); err != nil {
		return nil, err
	}

	return reply, nil
}

// OperatorRaftPeer supports actions on Raft peers. Currently we only support
// removing peers by address.
func (s *HTTPServer) OperatorRaftPeer(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	var args structs.RaftRemovePeerRequest
	s.parseDC(req, &args.Datacenter)
	s.parseToken(req, &args.Token)

	params := req.URL.Query()
	_, hasID := params["id"]
	if hasID {
		args.ID = raft.ServerID(params.Get("id"))
	}
	_, hasAddress := params["address"]
	if hasAddress {
		args.Address = raft.ServerAddress(params.Get("address"))
	}

	if !hasID && !hasAddress {
		return nil, BadRequestError{Reason: "Must specify either ?id with the server's ID or ?address with IP:port of peer to remove"}
	}
	if hasID && hasAddress {
		return nil, BadRequestError{Reason: "Must specify only one of ?id or ?address"}
	}

	var reply struct{}
	method := "Operator.RaftRemovePeerByID"
	if hasAddress {
		method = "Operator.RaftRemovePeerByAddress"
	}
	if err := s.agent.RPC(method, &args, &reply); err != nil {
		return nil, err
	}

	return nil, nil
}

type keyringArgs struct {
	Key         string
	Token       string
	RelayFactor uint8
	LocalOnly   bool // ?local-only; only used for GET requests
}

// OperatorKeyringEndpoint handles keyring operations (install, list, use, remove)
func (s *HTTPServer) OperatorKeyringEndpoint(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	var args keyringArgs
	if req.Method == "POST" || req.Method == "PUT" || req.Method == "DELETE" {
		if err := decodeBody(req, &args, nil); err != nil {
			return nil, BadRequestError{Reason: fmt.Sprintf("Request decode failed: %v", err)}
		}
	}
	s.parseToken(req, &args.Token)

	// Parse relay factor
	if relayFactor := req.URL.Query().Get("relay-factor"); relayFactor != "" {
		n, err := strconv.Atoi(relayFactor)
		if err != nil {
			return nil, BadRequestError{Reason: fmt.Sprintf("Error parsing relay factor: %v", err)}
		}

		args.RelayFactor, err = ParseRelayFactor(n)
		if err != nil {
			return nil, BadRequestError{Reason: fmt.Sprintf("Invalid relay-factor: %v", err)}
		}
	}

	// Parse local-only. local-only can only be used in GET requests.
	if localOnly := req.URL.Query().Get("local-only"); localOnly != "" {
		var err error
		args.LocalOnly, err = strconv.ParseBool(localOnly)
		if err != nil {
			return nil, BadRequestError{Reason: fmt.Sprintf("Error parsing local-only: %v", err)}
		}

		err = ValidateLocalOnly(args.LocalOnly, req.Method == "GET")
		if err != nil {
			return nil, BadRequestError{Reason: fmt.Sprintf("Invalid use of local-only: %v", err)}
		}
	}

	// Switch on the method
	switch req.Method {
	case "GET":
		return s.KeyringList(resp, req, &args)
	case "POST":
		return s.KeyringInstall(resp, req, &args)
	case "PUT":
		return s.KeyringUse(resp, req, &args)
	case "DELETE":
		return s.KeyringRemove(resp, req, &args)
	default:
		return nil, MethodNotAllowedError{req.Method, []string{"GET", "POST", "PUT", "DELETE"}}
	}
}

// KeyringInstall is used to install a new gossip encryption key into the cluster
func (s *HTTPServer) KeyringInstall(resp http.ResponseWriter, req *http.Request, args *keyringArgs) (interface{}, error) {
	responses, err := s.agent.InstallKey(args.Key, args.Token, args.RelayFactor)
	if err != nil {
		return nil, err
	}

	return nil, keyringErrorsOrNil(responses.Responses)
}

// KeyringList is used to list the keys installed in the cluster
func (s *HTTPServer) KeyringList(resp http.ResponseWriter, req *http.Request, args *keyringArgs) (interface{}, error) {
	responses, err := s.agent.ListKeys(args.Token, args.RelayFactor)
	if err != nil {
		return nil, err
	}

	return responses.Responses, keyringErrorsOrNil(responses.Responses)
}

// KeyringRemove is used to list the keys installed in the cluster
func (s *HTTPServer) KeyringRemove(resp http.ResponseWriter, req *http.Request, args *keyringArgs) (interface{}, error) {
	responses, err := s.agent.RemoveKey(args.Key, args.Token, args.RelayFactor)
	if err != nil {
		return nil, err
	}

	return nil, keyringErrorsOrNil(responses.Responses)
}

// KeyringUse is used to change the primary gossip encryption key
func (s *HTTPServer) KeyringUse(resp http.ResponseWriter, req *http.Request, args *keyringArgs) (interface{}, error) {
	responses, err := s.agent.UseKey(args.Key, args.Token, args.RelayFactor)
	if err != nil {
		return nil, err
	}

	return nil, keyringErrorsOrNil(responses.Responses)
}

func keyringErrorsOrNil(responses []*structs.KeyringResponse) error {
	var errs error
	for _, response := range responses {
		if response.Error != "" {
			pool := response.Datacenter + " (LAN)"
			if response.WAN {
				pool = "WAN"
			}
			errs = multierror.Append(errs, fmt.Errorf("%s error: %s", pool, response.Error))
			for key, message := range response.Messages {
				errs = multierror.Append(errs, fmt.Errorf("%s: %s", key, message))
			}
		}
	}
	return errs
}

// OperatorAutopilotConfiguration is used to inspect the current Autopilot configuration.
// This supports the stale query mode in case the cluster doesn't have a leader.
func (s *HTTPServer) OperatorAutopilotConfiguration(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	// Switch on the method
	switch req.Method {
	case "GET":
		var args structs.DCSpecificRequest
		if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
			return nil, nil
		}

		var reply autopilot.Config
		if err := s.agent.RPC("Operator.AutopilotGetConfiguration", &args, &reply); err != nil {
			return nil, err
		}

		out := api.AutopilotConfiguration{
			CleanupDeadServers:      reply.CleanupDeadServers,
			LastContactThreshold:    api.NewReadableDuration(reply.LastContactThreshold),
			MaxTrailingLogs:         reply.MaxTrailingLogs,
			ServerStabilizationTime: api.NewReadableDuration(reply.ServerStabilizationTime),
			RedundancyZoneTag:       reply.RedundancyZoneTag,
			DisableUpgradeMigration: reply.DisableUpgradeMigration,
			UpgradeVersionTag:       reply.UpgradeVersionTag,
			CreateIndex:             reply.CreateIndex,
			ModifyIndex:             reply.ModifyIndex,
		}

		return out, nil

	case "PUT":
		var args structs.AutopilotSetConfigRequest
		s.parseDC(req, &args.Datacenter)
		s.parseToken(req, &args.Token)

		var conf api.AutopilotConfiguration
		durations := NewDurationFixer("lastcontactthreshold", "serverstabilizationtime")
		if err := decodeBody(req, &conf, durations.FixupDurations); err != nil {
			return nil, BadRequestError{Reason: fmt.Sprintf("Error parsing autopilot config: %v", err)}
		}

		args.Config = autopilot.Config{
			CleanupDeadServers:      conf.CleanupDeadServers,
			LastContactThreshold:    conf.LastContactThreshold.Duration(),
			MaxTrailingLogs:         conf.MaxTrailingLogs,
			ServerStabilizationTime: conf.ServerStabilizationTime.Duration(),
			RedundancyZoneTag:       conf.RedundancyZoneTag,
			DisableUpgradeMigration: conf.DisableUpgradeMigration,
			UpgradeVersionTag:       conf.UpgradeVersionTag,
		}

		// Check for cas value
		params := req.URL.Query()
		if _, ok := params["cas"]; ok {
			casVal, err := strconv.ParseUint(params.Get("cas"), 10, 64)
			if err != nil {
				return nil, BadRequestError{Reason: fmt.Sprintf("Error parsing cas value: %v", err)}
			}
			args.Config.ModifyIndex = casVal
			args.CAS = true
		}

		var reply bool
		if err := s.agent.RPC("Operator.AutopilotSetConfiguration", &args, &reply); err != nil {
			return nil, err
		}

		// Only use the out value if this was a CAS
		if !args.CAS {
			return true, nil
		}
		return reply, nil

	default:
		return nil, MethodNotAllowedError{req.Method, []string{"GET", "PUT"}}
	}
}

// OperatorServerHealth is used to get the health of the servers in the local DC
func (s *HTTPServer) OperatorServerHealth(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	var args structs.DCSpecificRequest
	if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
		return nil, nil
	}

	var reply autopilot.OperatorHealthReply
	if err := s.agent.RPC("Operator.ServerHealth", &args, &reply); err != nil {
		return nil, err
	}

	// Reply with status 429 if something is unhealthy
	if !reply.Healthy {
		resp.WriteHeader(http.StatusTooManyRequests)
	}

	out := &api.OperatorHealthReply{
		Healthy:          reply.Healthy,
		FailureTolerance: reply.FailureTolerance,
	}
	for _, server := range reply.Servers {
		out.Servers = append(out.Servers, api.ServerHealth{
			ID:          server.ID,
			Name:        server.Name,
			Address:     server.Address,
			Version:     server.Version,
			Leader:      server.Leader,
			SerfStatus:  server.SerfStatus.String(),
			LastContact: api.NewReadableDuration(server.LastContact),
			LastTerm:    server.LastTerm,
			LastIndex:   server.LastIndex,
			Healthy:     server.Healthy,
			Voter:       server.Voter,
			StableSince: server.StableSince.Round(time.Second).UTC(),
		})
	}

	return out, nil
}
