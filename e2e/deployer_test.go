package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/celestiaorg/orchestrator-relayer/evm"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/stretchr/testify/assert"
)

func TestDeployer(t *testing.T) {
	if os.Getenv("QGB_INTEGRATION_TEST") != TRUE {
		t.Skip("Skipping QGB integration tests")
	}

	network, err := NewQGBNetwork()
	HandleNetworkError(t, network, err, false)

	// to release resources after tests
	defer network.DeleteAll() //nolint:errcheck

	err = network.StartMultiple(Core0, Ganache)
	HandleNetworkError(t, network, err, false)

	ctx := context.Background()

	err = network.WaitForBlock(ctx, 2)
	HandleNetworkError(t, network, err, false)

	_, err = network.GetLatestDeployedQGBContractWithCustomTimeout(ctx, 15*time.Second)
	HandleNetworkError(t, network, err, true)

	err = network.DeployQGBContract()
	HandleNetworkError(t, network, err, false)

	bridge, err := network.GetLatestDeployedQGBContract(ctx)
	HandleNetworkError(t, network, err, false)

	evmClient := evm.NewClient(nil, bridge, nil, nil, network.EVMRPC, evm.DefaultEVMGasLimit)

	eventNonce, err := evmClient.StateLastEventNonce(&bind.CallOpts{Context: ctx})
	assert.NoError(t, err)
	assert.Equal(t, uint64(1), eventNonce)
}
