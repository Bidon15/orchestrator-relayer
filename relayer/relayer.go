package relayer

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/ipfs/go-datastore"
	badger "github.com/ipfs/go-ds-badger2"

	"github.com/pkg/errors"

	"github.com/celestiaorg/orchestrator-relayer/helpers"

	coregethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"

	celestiatypes "github.com/celestiaorg/celestia-app/x/qgb/types"
	"github.com/celestiaorg/orchestrator-relayer/evm"
	"github.com/celestiaorg/orchestrator-relayer/p2p"
	"github.com/celestiaorg/orchestrator-relayer/rpc"
	"github.com/celestiaorg/orchestrator-relayer/types"
	wrapper "github.com/celestiaorg/quantum-gravity-bridge/wrappers/QuantumGravityBridge.sol"
	ethcmn "github.com/ethereum/go-ethereum/common"
	tmlog "github.com/tendermint/tendermint/libs/log"
)

type Relayer struct {
	TmQuerier      *rpc.TmQuerier
	AppQuerier     *rpc.AppQuerier
	P2PQuerier     *p2p.Querier
	EVMClient      *evm.Client
	logger         tmlog.Logger
	Retrier        *helpers.Retrier
	SignatureStore *badger.Datastore
}

func NewRelayer(
	tmQuerier *rpc.TmQuerier,
	appQuerier *rpc.AppQuerier,
	p2pQuerier *p2p.Querier,
	evmClient *evm.Client,
	logger tmlog.Logger,
	retrier *helpers.Retrier,
	sigStore *badger.Datastore,
) *Relayer {
	return &Relayer{
		TmQuerier:      tmQuerier,
		AppQuerier:     appQuerier,
		P2PQuerier:     p2pQuerier,
		EVMClient:      evmClient,
		logger:         logger,
		Retrier:        retrier,
		SignatureStore: sigStore,
	}
}

func (r *Relayer) Start(ctx context.Context) error {
	ethClient, err := r.EVMClient.NewEthClient()
	if err != nil {
		r.logger.Error(err.Error())
		return err
	}
	defer ethClient.Close()

	processFunc := func() error {
		// this function will relay attestations as long as there are confirms. And, after the contract is
		// up-to-date with the chain, it will stop.
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				lastContractNonce, err := r.EVMClient.StateLastEventNonce(&bind.CallOpts{})
				if err != nil {
					return err
				}

				latestNonce, err := r.AppQuerier.QueryLatestAttestationNonce(ctx)
				if err != nil {
					return err
				}

				// If the contract has already the last version, no need to relay anything
				if lastContractNonce >= latestNonce {
					r.logger.Debug("waiting for new nonce", "current_contract_nonce", lastContractNonce)
					return nil
				}

				att, err := r.AppQuerier.QueryAttestationByNonce(ctx, lastContractNonce+1)
				if err != nil {
					return err
				}
				if att == nil {
					return ErrAttestationNotFound
				}

				opts, err := r.EVMClient.NewTransactionOpts(ctx)
				if err != nil {
					return err
				}

				tx, err := r.ProcessAttestation(ctx, opts, att)
				if err != nil {
					return err
				}

				// wait for transaction to be mined
				_, err = r.EVMClient.WaitForTransaction(ctx, ethClient, tx)
				if err != nil {
					return err
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// using an immediate ticker not to wait the initial wait period before starting to relay
			err := helpers.ImmediateTicker(
				ctx,
				100*time.Second,
				processFunc,
			)
			if err != nil {
				// if an error occurs, retry a few times before exiting
				r.logger.Error(err.Error())
				err = r.Retrier.Retry(ctx, processFunc)
				if err != nil {
					return err
				}
			}
		}
	}
}

func (r *Relayer) ProcessAttestation(ctx context.Context, opts *bind.TransactOpts, attI celestiatypes.AttestationRequestI) (*coregethtypes.Transaction, error) {
	switch att := attI.(type) {
	case *celestiatypes.Valset:
		previousValset, err := r.AppQuerier.QueryLastValsetBeforeNonce(ctx, att.Nonce)
		if err != nil {
			return nil, err
		}
		signBytes, err := att.SignBytes()
		if err != nil {
			return nil, err
		}
		confirms, err := r.P2PQuerier.QueryTwoThirdsValsetConfirms(ctx, 30*time.Minute, 10*time.Second, att.Nonce, *previousValset, signBytes.Hex())
		if err != nil {
			return nil, err
		}
		err = r.SaveValsetSignaturesToStore(ctx, *att, confirms)
		if err != nil {
			return nil, err
		}
		tx, err := r.UpdateValidatorSet(ctx, opts, *att, att.TwoThirdsThreshold(), confirms)
		if err != nil {
			return nil, err
		}
		return tx, nil
	case *celestiatypes.DataCommitment:
		valset, err := r.AppQuerier.QueryLastValsetBeforeNonce(ctx, att.Nonce)
		if err != nil {
			return nil, err
		}
		commitment, err := r.TmQuerier.QueryCommitment(ctx, att.BeginBlock, att.EndBlock)
		if err != nil {
			return nil, err
		}
		dataRootHash := types.DataCommitmentTupleRootSignBytes(big.NewInt(int64(att.Nonce)), commitment)
		confirms, err := r.P2PQuerier.QueryTwoThirdsDataCommitmentConfirms(ctx, 30*time.Minute, 10*time.Second, *valset, att.Nonce, dataRootHash.Hex())
		if err != nil {
			return nil, err
		}
		err = r.SaveDataCommitmentSignaturesToStore(ctx, *att, dataRootHash.String(), confirms)
		if err != nil {
			return nil, err
		}
		tx, err := r.SubmitDataRootTupleRoot(opts, *att, *valset, commitment.String(), confirms)
		if err != nil {
			return nil, err
		}
		return tx, nil
	default:
		return nil, errors.Wrap(types.ErrUnknownAttestationType, strconv.FormatUint(attI.GetNonce(), 10))
	}
}

func (r *Relayer) UpdateValidatorSet(
	ctx context.Context,
	opts *bind.TransactOpts,
	valset celestiatypes.Valset,
	newThreshold uint64,
	confirms []types.ValsetConfirm,
) (*coregethtypes.Transaction, error) {
	var currentValset celestiatypes.Valset
	if valset.Nonce == 1 {
		currentValset = valset
	} else {
		vs, err := r.AppQuerier.QueryLastValsetBeforeNonce(ctx, valset.Nonce)
		if err != nil {
			return nil, err
		}
		currentValset = *vs
	}

	sigsMap := make(map[string]string)
	// to fetch the signatures easily by eth address
	for _, c := range confirms {
		sigsMap[c.EthAddress] = c.Signature
	}

	sigs, err := matchAttestationConfirmSigs(sigsMap, currentValset)
	if err != nil {
		return nil, err
	}

	tx, err := r.EVMClient.UpdateValidatorSet(
		opts,
		valset.Nonce,
		newThreshold,
		currentValset,
		valset,
		sigs,
	)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

func (r *Relayer) SubmitDataRootTupleRoot(
	opts *bind.TransactOpts,
	dataCommitment celestiatypes.DataCommitment,
	currentValset celestiatypes.Valset,
	commitment string,
	confirms []types.DataCommitmentConfirm,
) (*coregethtypes.Transaction, error) {
	sigsMap := make(map[string]string)
	// to fetch the signatures easily by eth address
	for _, c := range confirms {
		sigsMap[c.EthAddress] = c.Signature
	}

	sigs, err := matchAttestationConfirmSigs(sigsMap, currentValset)
	if err != nil {
		return nil, err
	}

	r.logger.Info(fmt.Sprintf(
		"relaying data commitment %d-%d...",
		dataCommitment.BeginBlock,
		dataCommitment.EndBlock,
	))

	tx, err := r.EVMClient.SubmitDataRootTupleRoot(
		opts,
		ethcmn.HexToHash(commitment),
		dataCommitment.Nonce,
		currentValset,
		sigs,
	)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

func (r *Relayer) SaveValsetSignaturesToStore(ctx context.Context, att celestiatypes.Valset, confirms []types.ValsetConfirm) error {
	batch, err := r.SignatureStore.Batch(ctx)
	if err != nil {
		return err
	}
	signBytes, err := att.SignBytes()
	if err != nil {
		return err
	}
	for _, confirm := range confirms {
		key := datastore.NewKey(p2p.GetValsetConfirmKey(att.Nonce, confirm.EthAddress, signBytes.Hex()))
		value, err := types.MarshalValsetConfirm(confirm)
		if err != nil {
			return err
		}
		has, err := r.SignatureStore.Has(ctx, key)
		if err != nil {
			return err
		}
		if !has {
			err := batch.Put(ctx, key, value)
			if err != nil {
				return err
			}
		}
	}
	return batch.Commit(ctx)
}

func (r *Relayer) SaveDataCommitmentSignaturesToStore(ctx context.Context, att celestiatypes.DataCommitment, dataRootTupleRoot string, confirms []types.DataCommitmentConfirm) error {
	batch, err := r.SignatureStore.Batch(ctx)
	if err != nil {
		return err
	}
	for _, confirm := range confirms {
		key := datastore.NewKey(p2p.GetDataCommitmentConfirmKey(att.Nonce, confirm.EthAddress, dataRootTupleRoot))
		value, err := types.MarshalDataCommitmentConfirm(confirm)
		if err != nil {
			return err
		}
		has, err := r.SignatureStore.Has(ctx, key)
		if err != nil {
			return err
		}
		if !has {
			err := batch.Put(ctx, key, value)
			if err != nil {
				return err
			}
		}
	}
	return batch.Commit(ctx)
}

// matchAttestationConfirmSigs matches and sorts the confirm signatures with the valset
// members as expected by the QGB contract.
// Also, it leaves the non provided signatures as nil in the `sigs` slice:
// https://github.com/celestiaorg/celestia-app/issues/628
func matchAttestationConfirmSigs(
	signatures map[string]string,
	currentValset celestiatypes.Valset,
) ([]wrapper.Signature, error) {
	sigs := make([]wrapper.Signature, len(currentValset.Members))
	// the QGB contract expects the signatures to be ordered by validators in valset
	for i, val := range currentValset.Members {
		sig, has := signatures[val.EvmAddress]
		if !has {
			continue
		}
		v, r, s, err := evm.SigToVRS(sig)
		if err != nil {
			return nil, err
		}

		sigs[i] = wrapper.Signature{
			V: v,
			R: r,
			S: s,
		}
	}

	return sigs, nil
}
