package avail

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/0xPolygon/polygon-edge/crypto"
	"github.com/0xPolygon/polygon-edge/types"
	stypes "github.com/centrifuge/go-substrate-rpc-client/v4/types"

	"github.com/maticnetwork/avail-settlement/pkg/block"
	"github.com/maticnetwork/avail-settlement/pkg/common"
	"github.com/maticnetwork/avail-settlement/pkg/staking"
)

func (sw *SequencerWorker) waitForStakedSequencer(activeParticipantsQuerier staking.ActiveSequencers, nodeAddr types.Address) bool {
	for {
		sequencerStaked, sequencerError := activeParticipantsQuerier.Contains(nodeAddr)
		if sequencerError != nil {
			sw.logger.Error("failed to check if my account is among active staked sequencers. Retrying in few seconds...", "error", sequencerError)
			time.Sleep(3 * time.Second)
			continue
		}

		if !sequencerStaked {
			sw.logger.Warn("my account is not among active staked sequencers. Retrying in few seconds...", "address", nodeAddr.String())
			time.Sleep(3 * time.Second)
			continue
		}
		break
	}
	return true
}

func (d *Avail) ensureStaked(wg *sync.WaitGroup, activeParticipantsQuerier staking.ActiveParticipants) error {
	var nodeType staking.NodeType

	switch d.nodeType {
	case BootstrapSequencer, Sequencer:
		nodeType = staking.Sequencer
	case WatchTower:
		nodeType = staking.WatchTower
	default:
		return fmt.Errorf("unknown node type: %q", d.nodeType)
	}

	var returnErr error

	go func() {
		for {
			inProbation, err := activeParticipantsQuerier.InProbation(d.minerAddr)
			if err != nil {
				d.logger.Error("Failed to check if participant is currently in probation... Rechecking again in few seconds...", "error", err)
				time.Sleep(3 * time.Second)
				continue
			}

			if inProbation {
				d.logger.Warn("Participant (node/miner) is currently in probation.... Rechecking again in few seconds...", "error", err)
				time.Sleep(5 * time.Second)
				continue
			}

			staked, err := activeParticipantsQuerier.Contains(d.minerAddr, nodeType)
			if err != nil {
				d.logger.Error("Failed to check if participant exists... Rechecking again in few seconds...", "error", err)
				time.Sleep(3 * time.Second)
				continue
			}

			if staked {
				d.logger.Info("Node is successfully staked... Rechecking in few seconds for potential changes...")
				time.Sleep(3 * time.Second)
				continue
			}

			switch MechanismType(d.nodeType) {
			case BootstrapSequencer:
				// Staking smart contract does not support `BootstrapSequencer` MachineType.
				returnErr = d.stakeParticipant(false, Sequencer.String())
			case Sequencer:
				staked, returnErr = d.stakeParticipantThroughTxPool(activeParticipantsQuerier)
				if staked {
					return
				}
			case WatchTower:
				staked, returnErr = d.stakeParticipantThroughTxPool(activeParticipantsQuerier)
				if staked {
					return
				}
			}
		}
	}()

	return returnErr
}

func (d *Avail) stakeParticipant(shouldWait bool, nodeType string) error {
	// Bootnode does not need to wait for any additional peers to be discovered prior pushing the
	// block towards rest of the community, however, sequencers and watchtowers must!
	if shouldWait {
		for {
			if d.network.GetBootnodeConnCount() > 0 {
				break
			}

			time.Sleep(1 * time.Second)
			continue
		}
	}

	// First, build the staking block.
	blockBuilderFactory := block.NewBlockBuilderFactory(d.blockchain, d.executor, d.logger)
	bb, err := blockBuilderFactory.FromBlockchainHead()
	if err != nil {
		return err
	}

	bb.SetCoinbaseAddress(d.minerAddr)
	bb.SignWith(d.signKey)

	stakeAmount := big.NewInt(0).Mul(big.NewInt(10), common.ETH)
	tx, err := staking.StakeTx(d.minerAddr, stakeAmount, nodeType, 1_000_000)
	if err != nil {
		return err
	}

	txSigner := &crypto.FrontierSigner{}
	tx, err = txSigner.SignTx(tx, d.signKey)
	if err != nil {
		return err
	}

	bb.AddTransactions(tx)
	blk, err := bb.Build()
	if err != nil {
		d.logger.Error("failed to build staking block", "node_type", nodeType, "error", err)
		return err
	}

	d.logger.Debug("sending block with staking tx to Avail")
	err = d.availSender.SendAndWaitForStatus(blk, stypes.ExtrinsicStatus{IsInBlock: true})
	if err != nil {
		d.logger.Error("error while submitting data to avail", "error", err)
		return err
	}

	d.logger.Info(
		"Successfully wrote staking block to the blockchain",
		"hash", blk.Hash().String(),
	)

	err = d.blockchain.WriteBlock(blk, d.nodeType.String())
	if err != nil {
		return err
	}

	return nil
}

func (d *Avail) stakeParticipantThroughTxPool(activeParticipantsQuerier staking.ActiveParticipants) (bool, error) {
	// We need to have at least one node available to be able successfully push tx to the neighborhood peers
	for {
		if d.network == nil || d.network.GetBootnodeConnCount() > 0 {
			break
		}

		time.Sleep(1 * time.Second)
		continue
	}

	// Apparently, we still need to wait a bit more time than boot node count to be able
	// process staking. If there's only bootstrap sequencer and one sequencer without this sleep
	// txpool tx will be added but bootstrap sequencer won't receive it.
	time.Sleep(5 * time.Second)

	stakeAmount := big.NewInt(0).Mul(big.NewInt(10), common.ETH)
	tx, err := staking.StakeTx(d.minerAddr, stakeAmount, d.nodeType.String(), 1_000_000)
	if err != nil {
		return false, err
	}

	txSigner := &crypto.FrontierSigner{}
	tx, err = txSigner.SignTx(tx, d.signKey)
	if err != nil {
		return false, err
	}

	for retries := 0; retries < 10; retries++ {
		// Submit staking transaction for execution by active sequencer.
		err = d.txpool.AddTx(tx)
		if err != nil {
			d.logger.Error("Failure to add staking tx to the txpool err: %s", err)
			time.Sleep(2 * time.Second)
			continue
		}

		break
	}

	if err != nil {
		return false, err
	}

	// Syncer will be syncing the blockchain in the background, so once an active
	// sequencer picks up the staking transaction from the txpool, it becomes
	// effective and visible to us as well, via blockchain.
	var staked bool
	for !staked {
		d.logger.Info("Submitted transaction, waiting for staking contract update...")
		staked, err = activeParticipantsQuerier.Contains(d.minerAddr, staking.NodeType(d.nodeType))
		if err != nil {
			return false, err
		}
		// Wait a bit before checking again.
		time.Sleep(3 * time.Second)
	}

	return true, nil
}
