package avail

import (
	avail_types "github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/maticnetwork/avail-settlement/consensus/avail/validator"
	"github.com/maticnetwork/avail-settlement/pkg/avail"
	"github.com/maticnetwork/avail-settlement/pkg/block"
)

func (d *Avail) getNextAvailBlockNumber() uint64 {
	head := d.blockchain.Header()

	/// We have new blockchain. Allow syncing from last to 1st block
	if head.Number == 0 {
		return 1
	}

	blk, err := d.availClient.SearchBlock(0, head.Number, d.syncFunc)
	if err != nil {
		d.logger.Error("failure to sync node", "error", err)
		return 0
	}

	return uint64(blk.Block.Header.Number)
}

func (d *Avail) syncNode() (uint64, error) {
	availNextBlockNumber := d.getNextAvailBlockNumber()

	hdr, err := d.availClient.GetLatestHeader()
	if err != nil {
		d.logger.Error("couldn't fetch latest block hash", "error", err)
		return 0, nil
	}

	callIdx, err := avail.FindCallIndex(d.availClient)
	if err != nil {
		return availNextBlockNumber, err
	}

	fraudResolver := NewFraudResolver(d.logger, d.blockchain, d.executor, d.txpool, nil, nil, d.minerAddr, d.signKey, d.availSender, d.nodeType)
	validator := validator.New(d.blockchain, d.minerAddr, d.logger)

	// BlockStream watcher must be started after the staking is done. Otherwise
	// the stream is out-of-sync.
	availBlockStream := d.availClient.BlockStream(availNextBlockNumber)
	defer availBlockStream.Close()

	for {
		var blk *avail_types.SignedBlock

		select {
		case blk = <-availBlockStream.Chan():

		case <-d.closeCh:
			if err := d.stakingNode.UnStake(d.signKey); err != nil {
				d.logger.Error("failed to unstake the node", "error", err)
				return availNextBlockNumber, nil
			}
			return 0, nil
		}

		edgeBlks, err := block.FromAvail(blk, d.availAppID, callIdx, d.logger)
		if len(edgeBlks) == 0 && err != nil {
			if err != block.ErrNoExtrinsicFound {
				d.logger.Warn("unexpected error while extracting SL blocks from Avail block", "error", err)
				continue
			}
		}

		// Write down blocks received from avail to make sure we're synced before processing with the
		// fraud check or writing down new blocks...
		for _, edgeBlk := range edgeBlks {
			if !fraudResolver.IsFraudProofBlock(edgeBlk) {
				if err := validator.Check(edgeBlk); err == nil {

					if len(edgeBlk.Transactions) > 0 {
						d.logger.Warn("WE HAVE TRANSACTIONS INSIDE")
					}

					if err := d.blockchain.WriteBlock(edgeBlk, d.nodeType.String()); err != nil {
						d.logger.Warn(
							"failed to write edge block received from avail",
							"edge_block_hash", edgeBlk.Hash(),
							"error", err,
						)
					}
				} else {
					d.logger.Warn(
						"failed to validate edge block received from avail",
						"edge_block_hash", edgeBlk.Hash(),
						"error", err,
					)
				}
			}
		}

		availNextBlockNumber = uint64(blk.Block.Header.Number)

		// Stop the loop if we are synced with latest header
		if hdr.Number == blk.Block.Header.Number {
			break
		}
	}

	return availNextBlockNumber, nil
}

// Searches for the edge block in the Avail and returns back avail block for future catch up by the node
func (d *Avail) syncFunc(availBlk *avail_types.SignedBlock, targetEdgeBlock uint64, callIdx avail_types.CallIndex) (int, bool, error) {
	blks, err := block.FromAvail(availBlk, d.availAppID, callIdx, d.logger)
	if err != nil && err != block.ErrNoExtrinsicFound {
		return -1, false, err
	}

	if blks == nil || len(blks) < 1 {
		return -1, false, nil
	}

	for _, blk := range blks {
		if blk.Header.Number == targetEdgeBlock {
			return int(availBlk.Block.Header.Number), true, nil
		}
	}

	return -1, false, nil
}