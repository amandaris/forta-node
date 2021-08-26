package registry

import (
	"encoding/json"
	"fmt"
	"github.com/forta-network/forta-node/services/registry/regtypes"
	"net/http"
)

type ipfsClient struct {
	gatewayURL string
}

func (client *ipfsClient) GetAgentFile(cid string) (*regtypes.AgentFile, error) {
	resp, err := http.Get(fmt.Sprintf("%s/ipfs/%s", client.gatewayURL, cid))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var agentData regtypes.AgentFile
	if err := json.NewDecoder(resp.Body).Decode(&agentData); err != nil {
		return nil, fmt.Errorf("failed to decode the agent file: %v", err)
	}
	return &agentData, nil
}
