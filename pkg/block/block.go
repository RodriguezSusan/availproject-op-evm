// Package block provides utilities for constructing and building blockchain blocks.
package block

import (
	"bytes"
	"errors"

	"github.com/0xPolygon/polygon-edge/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/scale"
	avail_types "github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/hashicorp/go-hclog"
	"github.com/maticnetwork/avail-settlement/pkg/avail"
)

// Constants defining different block sources
const (
	SourceAvail      = "Avail"
	SourceWatchTower = "WatchTower"
)

// Error returned when no compatible extrinsic is found in Avail block's extrinsic data
var ErrNoExtrinsicFound = errors.New("no compatible extrinsic found")

// FromAvail converts Avail blocks into Edge blocks.
// It takes an Avail block, appID, callIdx, and logger as parameters.
// It returns a slice of Edge blocks or an error if conversion fails.
func FromAvail(avail_blk *avail_types.SignedBlock, appID avail_types.UCompact, callIdx avail_types.CallIndex, logger hclog.Logger) ([]*types.Block, error) {
	toReturn := []*types.Block{}

	for i, extrinsic := range avail_blk.Block.Extrinsics {
		if extrinsic.Signature.AppID.Int64() != appID.Int64() {
			logger.Debug("block extrinsic's AppID doesn't match", "avail_block_number", avail_blk.Block.Header.Number, "extrinsic_index", i, "extrinsic_app_id", extrinsic.Signature.AppID, "filter_app_id", appID)
			continue
		}

		if extrinsic.Method.CallIndex != callIdx {
			logger.Debug("block extrinsic's Method.CallIndex doesn't match", "avail_block_number", avail_blk.Block.Header.Number, "extrinsic_index", i, "extrinsic_call_index", extrinsic.Method.CallIndex, "filter_call_index", callIdx)
			continue
		}

		var blob avail.Blob
		{
			var bs avail_types.Bytes
			err := codec.Decode(extrinsic.Method.Args, &bs)
			if err != nil {
				logger.Info("decoding block extrinsic's raw bytes from args failed", "avail_block_number", avail_blk.Block.Header.Number, "extrinsic_index", i, "error", err)
				continue
			}

			decoder := scale.NewDecoder(bytes.NewBuffer(bs))
			err = blob.Decode(*decoder)
			if err != nil {
				logger.Info("decoding blob from extrinsic data failed", "avail_block_number", avail_blk.Block.Header.Number, "extrinsic_index", i, "error", err)
				continue
			}
		}

		blk := types.Block{}
		if err := blk.UnmarshalRLP(blob.Data); err != nil {
			return nil, err
		}

		logger.Info("Received new edge block from avail.", "hash", blk.Header.Hash, "parent_hash", blk.Header.ParentHash, "avail_block_number", blk.Header.Number)

		toReturn = append(toReturn, &blk)
	}

	if len(toReturn) == 0 {
		return nil, ErrNoExtrinsicFound
	}

	return toReturn, nil
}
